# 07 — Engine landscape, and embedding a Rust library in a Go binary

**Question.** Are there published, reproducible comparisons of tantivy against Lucene, SQLite FTS5, Bleve and Bluge, Xapian, Zoekt, Meilisearch and others (indexing throughput, query latency, memory, disk, and on what corpora)? Is any engine likely to suit a local code-search product better than tantivy? What does it cost to embed a Rust library in a Go binary (cgo, a static library, a sidecar process), per call and per document, and who does it in production?

**Credibility scale.** **A**: peer-reviewed paper, the engine's own source code, or a reproducible benchmark with a published harness and published raw results. **B**: official documentation, or an engineering write-up by the team that runs the system. **C**: secondary or one-person material, used only to locate A or B sources or flagged as weak where nothing better exists.

All sources were read on 2026-10-08.

## Short answer

- **Only one benchmark is reproducible and multi-engine: the Search Benchmark Game.** It measures single-threaded query latency on English Wikipedia (about 5M documents) for one field, read-only. It does not measure indexing, disk, memory, deletes or multi-field scoring.
  - Its published run has tantivy 0.25, Lucene 10.3.0 (with and without document reordering), PISA 0.8.2 and iResearch 26.03.1.
  - Lucene 10.3 is now faster than tantivy on top-k queries.
  - tantivy is faster than Lucene on `TOP_100_COUNT`, the mode that visits every match.
- **No reproducible benchmark compares tantivy with Bleve, Bluge, Xapian, Zoekt or Meilisearch.**
  - Bleve and Bluge have engine directories in the Search Benchmark Game repository, but neither published result file contains them.
  - The only FTS5-against-tantivy numbers come from one person's two-commit repository (C). On BEIR FiQA, tantivy's median latency was 57× lower and its index 4.8× smaller, with nDCG@10 equal within 0.003.
- **No engine natively meets the codectx rules.** These rules are: complete answers, df over the union of the columns counting visible documents only, f64 BM25F with one saturation, a content-only total order, and retained snapshots. Every candidate would be used for its postings and its storage only, with codectx's own scoring on top, which is what the tantivy product-mode collector already does.
  - Among the libraries that can be embedded, tantivy has the best-documented storage and the most production embedders.
  - The serious alternative is staying where codectx is now (FTS5, with its own packed segments, in pure Go).
  - Bleve is the only pure-Go full-text library that is still maintained. No published evidence shows it competitive on speed.
- **Embedding costs.**
  - A cgo call costs about 30–40 ns (Go 1.21/1.22, measured by third parties). Go 1.26 reports about 30% less.
  - The cost that matters for a complete-answer collector is per-document marshalling: one call per hit, as in the tantivy-go binding, against one call per page or answer that returns a packed buffer.
  - The safety cost is larger than the speed cost. A Rust panic across `extern "C"` aborts the whole process (Rust ≥ 1.81), and a blocking call occupies an OS thread.
  - Milvus is the production precedent for a Go binary that calls tantivy, as a Rust `staticlib` behind C++ called through cgo. Anytype is the precedent for a Go binary that calls tantivy through cgo directly.

---

## 1. Reproducible benchmarks

### 1.1 Search Benchmark Game (quickwit-oss): the only multi-engine reproducible benchmark

- **Source.** *search-benchmark-game*, quickwit-oss (tantivy's maintainers), repository last changed 2026-09-05. https://github.com/quickwit-oss/search-benchmark-game. Published results: https://tantivy-search.github.io/bench/ (raw file `results.json`, downloaded and analysed here). **Credibility A** for reproducibility: the harness, the `make corpus/index/compile/bench` steps and the raw per-query timings are public. It is a **B** for neutrality, because the tantivy team runs it.
- **What it measures (README, A).**
  - The corpus is "the English wikipedia", with stemming disabled.
  - The queries are "derived from the AOL query dataset". They are intersections, unions, phrases and some single terms.
  - The collection modes are `COUNT`, `TOP_10`, `TOP_100`, `TOP_1000` and `TOP_100_COUNT`.
  - Each run starts with a warm-up "so that all of the data is loaded and in page cache". Runs are single-threaded.
  - Hardware: AMD Ryzen 7 9800X3D, Rust 1.96.0, OpenJDK 21.0.11.
- **What it does not measure.**
  - In Adrien Grand's analysis (§1.2): "The benchmark does not report index size or indexing time." The data is read-only with no deletions, every query targets one field, and "all data fits in the page cache".
  - So it says nothing about deletes and statistics (rule 1), multi-field scoring (rule 7), disk, memory or incremental updates.
- **Engines in the repository** (the `engines/` listing): bleve-0.8.0 (boltdb and scorch), bluge-0.2.2, iresearch-26.03.1, Lucene 7.2.1 through 10.4.0 (and `-bp` variants), pisa-0.8.2, rucene-0.1, and tantivy 0.6 through 0.26 plus main.
- **Engines in the published results.** The live `results.json` holds tantivy-0.25, lucene-10.3.0, lucene-10.3.0-bp, pisa-0.8.2 and iresearch-26.03.1. An older results file at quickwit-oss.github.io holds only tantivy-0.16 and Lucene 8.10.1. **Neither contains Bleve or Bluge.**
- **Numbers from the raw file.** The table below is computed here, not taken from a page. Each query's timing is the mean of its 10 runs. The aggregate is over the 661 queries that every engine answered (PISA skips phrases, answering 661 of 962). Units are µs.

| mode | tantivy 0.25 mean / median | Lucene 10.3 mean / median | Lucene 10.3-bp | PISA 0.8.2 | iResearch 26.03.1 |
|---|---|---|---|---|---|
| TOP_10 | 766 / 311 | 405 / 204 | 309 / 162 | 468 / 171 | 414 / 206 |
| TOP_100_COUNT (visits every match) | 1 935 / 640 | 2 673 / 706 | 2 708 / 516 | 1 550 / 252 | 887 / 441 |
| COUNT | 633 / 163 | 355 / 193 | 291 / 138 | 1 482 / 236 | 263 / 150 |

- **How each engine is built.** The engines' own `details` entries in `results.json` (A, as self-description):
  - tantivy uses "block-max WAND". Its "impacts … approximation only the optimal (term_freq, norm) pair for the block" are "computed assuming the current segment statistics".
  - Lucene uses "vectorized variants of block-max MAXSCORE".
  - PISA's "impacts are stored directly, and require a single segment".
  - iResearch is "written in C++ to power SereneDB". Its standalone repository, iresearch-toolkit/iresearch, is archived (GitHub API, `archived=true`).
- **What it means.** On exhaustive evaluation, the closest analogue to codectx's complete answers (rule 3), tantivy beats Lucene and trails iResearch. On pruned top-k, Lucene is now ahead. Absolute latencies on a 5M-document corpus are below 2 ms on average in every mode.

### 1.2 Adrien Grand's analyses of the benchmark (Lucene committer)

- *Why is Tantivy faster than Lucene?*, Adrien Grand, 2025-04-12, edited 2025-11-12. https://jpountz.github.io/2025/04/12/why-is-Tantivy-faster-than-Lucene.html. **Credibility B**: a Lucene committer writing about changes he made, each with a measured nightly-benchmark gain. He is an interested party on the Lucene side.
  - The reasons he gives are popcount counting, LLVM auto-vectorisation, simpler skip data, megamorphic call sites in Lucene, and tantivy loading all positions of a document at once for phrases.
  - The edit reads: "Apache Lucene is now on par with Tantivy on most queries and even faster on some queries."
- *An analysis of Search Benchmark, the Game*, Adrien Grand, 2025-05-12. https://jpountz.github.io/2025/05/12/analysis-of-Search-Benchmark-the-Game.html. **B**. It contrasts block-max WAND (tantivy, PISA), which "can skip more documents, but comes with a higher overhead", with Lucene's MAXSCORE, which is faster on long queries. It also lists the benchmark's blind spots quoted above.

### 1.3 SQLite FTS5 against tantivy on BEIR (the only such comparison found)

- *search-without-elasticsearch*, GitHub user nazmul284, measured 2026-10-07 on an 8 GB Apple M2, Python harness (`harness/bench.py`), two commits, engine versions not stated. https://github.com/nazmul284/search-without-elasticsearch. **Credibility C**: the harness is published, but there is one author and one run, the versions are unstated, Python bindings sit inside the timing, and each engine uses its default English analysis.

| corpus | engine | nDCG@10 | R@100 | p50 ms | p95 ms | build s | MB |
|---|---|---|---|---|---|---|---|
| SciFact (5 183 docs) | FTS5 | 0.682 | 0.925 | 4.90 | 12.69 | 0.19 | 17.7 |
| SciFact | tantivy | 0.665 | 0.908 | 0.23 | 0.38 | 0.58 | 2.8 |
| FiQA (57 638 docs) | FTS5 | 0.248 | 0.555 | 42.10 | 85.87 | 1.76 | 84.1 |
| FiQA | tantivy | 0.251 | 0.553 | 0.74 | 1.13 | 1.11 | 17.5 |

- **What it means.** Ranking quality is a wash, because both use BM25. Latency and disk favour tantivy by one to two orders of magnitude. The direction agrees with codectx's own measured per-posting cost on SQLite tables (ADR-0007); the magnitude is unverified.

### 1.4 Searched for and not found

- No reproducible benchmark of Bleve or Bluge against tantivy or Lucene. Searches in extended mode returned only an informal 2020 blog post by Alexander Reelsen, https://spinscale.de/posts/2020-10-20-search-engines-and-libraries-overview.html (**C**, single user, not like-for-like). The tantivy-go README calls Bleve "surprisingly slow compared to Tantivy" without numbers (**C** as evidence).
- No benchmark of Xapian, Zoekt or Meilisearch against tantivy.
- No reproducible indexing-throughput or index-size comparison of tantivy and Lucene. tantivy issue #576 asks for one: https://github.com/quickwit-oss/tantivy/issues/576.

---

## 2. Engine properties that decide fit (from each engine's source or official docs)

| engine | language / license | scoring model as shipped | multi-field | snapshots / updates | caps and other notes |
|---|---|---|---|---|---|
| **tantivy 0.26.2** | Rust / MIT (GitHub API). 0.26.2 published 2026-09-08 (crates.io) | Per-field BM25, f32, k1=1.2, b=0.75 (crate source; see `../features/06-scoring.md`) | No BM25F: one `Bm25Weight` per field. Summing fields means separate saturation | A held `Searcher` is a snapshot. Deletes are a bitset until a merge | Stock `TopDocs` is top-k; a custom collector can keep every hit |
| **SQLite FTS5** (incumbent) | C / public domain; codectx uses the pure-Go `modernc.org/sqlite` | `bm25()` sums **column-weighted** term frequencies, then applies **one** saturation with D = the row's total tokens. idf = log((N−nHit+0.5)/(nHit+0.5)), floored at 1e-6, f64 (`ext/fts5/fts5_aux.c`, lines 630–753) | Yes: weights per column. nHit = rows matching the phrase in any column | SQLite transactions and WAL. Merges are incremental (`automerge`, default 4; `crisismerge`, 16) | `detail=full` index 743 MiB for 1 636 MiB of mail (FTS5 docs) |
| **Bleve v2.6.1** (2026-08-24) | Go / Apache-2.0 | TF-IDF by default; BM25 from v2.5 through `indexMapping.ScoringModel = "bm25"` | "the scoring itself is done independently for each field and then aggregated" (docs/scoring.md) | scorch segments. "Global scoring" is a two-phase statistics round trip across shards | Pure Go, no cgo |
| **Bluge v0.2.2** | Go / Apache-2.0 | BM25 in f64. **Defect in source:** `math.Log(1.0 + float64(docCount-docFreq) + 0.5/(float64(docFreq)+0.5))` puts the +0.5 outside the ratio (`search/similarity/bm25.go:52`) | Per field | Same lineage as scorch | Last release 2022-07-04 and last commit 2022-07-04 (GitHub API): unmaintained |
| **Zoekt** | Go / Apache-2.0 | Positional trigrams, regex. An optional `UseBM25Scoring` sums term frequencies in which filename and symbol matches count `importantTermBoost`× and test or generated files are divided down. It is one saturation with length = file bytes / mean, and **idf is constant** ("idf can down-weight the score of some keywords too much", `index/score.go:355-386`) | Filename and symbol folded into tf (BM25F-shaped) | Shards rebuilt per repository. Shard merging exists | "about 3x the corpus size" index ("3.5x" with metadata), positional trigrams need "1.2x corpus size of RAM" (doc/design.md). Sourcegraph cut RAM from 1 400 KB to 310 KB per repository (§3) |
| **Xapian 1.4/2.x** | C++ / **GPL v2+** (COPYING; xapian.org) | BM25Weight, defaults k1=1, b=0.5, length = **whole document** (API docs) | Prefixed terms; no per-field length | Single writer. A reader holds a snapshot, but "at most two versions can exist concurrently", and older readers get `DatabaseModifiedError` (getting-started docs) | GPL is a distribution constraint for an Apache-2.0 binary |
| **Meilisearch** | Rust server / not OSI-plain (GitHub API: NOASSERTION) | Bucket sort of ranking rules (words, typo, proximity, attributeRank, sort, wordPosition, exactness); BM25 is not mentioned | Attribute rank | Server process | `maxTotalHits` default **1000**: "Limit and offset cannot go beyond this value" (settings reference) |
| **PISA 0.8.2** | C++ / Apache-2.0 | BM25 with block-max WAND | Single field | "an *in-memory* system"; impacts "require a single segment" | A research engine (OSIRRC 2019 paper); no phrases in the benchmark |
| **Lucene 10.x** | Java / Apache-2.0 | BM25; `CombinedFieldQuery` for BM25F (see `01-field-combination`; precision in `05`) | Yes, see 01 | `IndexDeletionPolicy` keeps "older point-in-time commits" (`SnapshotDeletionPolicy`, `KeepLastNCommitsDeletionPolicy`, `NoDeletionPolicy`; 10.3 javadoc) | Needs a JVM next to a Go binary |

**Credibility.** Every row is **A** (the engine's source) or **B** (its official docs), except the license entries taken from the GitHub API, which are **B**.

---

## 3. Production users and code-search engines (B: write-ups by the teams that run them)

- **GitHub Blackbird.** *The technology behind GitHub's new code search*, Timothy Clem, GitHub, 2023-02-06. https://github.blog/engineering/architecture-optimization/the-technology-behind-githubs-new-code-search/. **B.**
  - A custom Rust engine with "dynamic gram sizes instead of trigrams".
  - 115 TB of code and 15.5 billion documents, deduplicated to "around 28 TB of unique content", give a 25 TB index.
  - Ingest is "around 120,000 documents per second". Per-shard p99 is "on the order of 100 ms". A 64-core host tops out at about 640 QPS.
  - The service returns the top 100 by score.
  - It was built because general text search had "poor" code UX, slow indexing and high cost: Elasticsearch took "months" for about 8 million repositories.
- **Sourcegraph Zoekt memory.** *A 5x reduction in RAM usage with Zoekt memory optimizations*, Ryan Hitchman, Sourcegraph, 2021-08-19. https://sourcegraph.com/blog/zoekt-memory-optimizations-for-sourcegraph-cloud. **B.**
  - On 19 000 repositories (2.6 billion lines, 166 GB), live heap fell from 22 GB to 4 GB, "from 1400KB of RAM per repo to 310KB with no measurable latency changes".
  - The cuts came from sorted arrays in place of maps, split 32-bit ngrams and compressed filename postings.
- **ParadeDB (pg_search).** *A New Postgres Block Storage Layout for Full Text Search*, Ming Ying, ParadeDB, 2025-01-16. https://www.paradedb.com/blog/block-storage-part-one. **B.**
  - tantivy segment files live in Postgres blocks.
  - "Postgres MVCC visibility information is stored alongside each segment UUID", and the extension "uses MVCC visibility rules to construct a snapshot of the list of all visible segments".
  - Merges are serialised through a transaction id in a metadata block.
  - They run a tantivy fork (paradedb/tantivy, e.g. PR #275, lazy posting reads).
  - **This is the production precedent for owning snapshot visibility outside tantivy (rule 5, rule 6).**
- **Turso.** *Beyond FTS5: Building Transactional Full-Text Search in TursoDB*, Preston Thorpe, Turso, 2026-01-27. https://turso.tech/blog/beyond-fts5. **B.**
  - FTS5 is "full of caveats and shortcomings"; the post gives no specifics.
  - tantivy runs through a custom `Directory` over the database B-tree, in 512 KB chunks.
  - Because storage is single-writer, "merges become dangerous", so it runs `NoMergePolicy` by default and the user runs `OPTIMIZE INDEX`.
  - Per-column weights (`weights = 'title=2.0,body=1.0'`). The feature is experimental, and the post has no benchmarks.
- **Quickwit.** *Quickwit joins Datadog*, Quickwit, 2025-01-09. https://quickwit.io/blog/quickwit-joins-datadog. **B.** tantivy's maintainers were acquired, and the post promises continued open-source releases.
  - Releases since then: 0.24.2 (2025-07), 0.25.0 (2025-08), 0.26.0 (2026-03-31), 0.26.1 (2026-04-21) and 0.26.2 (2026-09-08), from crates.io. Upstream commits were still landing on 2026-10-08 (GitHub API `pushed_at`).
  - **Maintenance risk is low on this evidence.**

---

## 4. Embedding a Rust library in a Go binary

### 4.1 Per-call cost of cgo

- **Go 1.26 release notes**, Go team, 2026-02. https://go.dev/doc/go1.26. **B**: "The baseline runtime overhead of cgo calls has been reduced by ~30%."
- **Go 1.21 release notes**, Go team, 2023-08. https://go.dev/doc/go1.21. **B**: C-to-Go callbacks on C-created threads fell "from ~1-3 microseconds per call to ~100-200 nanoseconds per call". That is the callback direction only, which matters if Rust calls back into Go per hit.
- *CGO Performance In Go 1.21*, Shane (shane.ai), 2023-09-01. https://shane.ai/posts/cgo-performance-in-go1.21/. **C**: a personal blog with the benchmark code shown and slightly inconsistent tables. A cgo call took 38.93 ns/op on one core and about 4.9 ns/op on 16 cores in parallel, against 1.665 ns for a non-inlined Go call.
- *Rust2Go Part 2*, ihcblog, about 2024. https://en.ihcblog.com/rust2go-cgo-asm/. **C**: cgo took 28.48 ns/op on Go 1.18 and 29.19 ns/op on Go 1.22, on a Ryzen 7 7840HS.
- *The cost and complexity of Cgo*, Tobias Grieger, Cockroach Labs, 2015-12-10. https://www.cockroachlabs.com/blog/the-cost-and-complexity-of-cgo/. **B**, but dated (Go 1.5 era).
  - 171 ns/op against 1.83 ns/op for Go.
  - "A blocking cgo call occupies a system thread". They batched work into C to cut boundary crossings.
  - Static builds and cross-compilation get harder, and pprof and stack traces degrade at the boundary.
- **cmd/cgo**, Go docs. https://pkg.go.dev/cmd/cgo. **B**: "Go code may pass a Go pointer to C provided the memory to which it points does not contain any Go pointers", and "C code may not keep a copy of a Go pointer after the call returns, unless the memory it points to is pinned". The default `cgocheck=1` enforces this dynamically.

### 4.2 Safety cost

- **Rust 1.81 announcement**, Rust project, 2024-09-05. https://blog.rust-lang.org/2024/09/05/Rust-1.81.0/. **B**: "the non-unwind ABIs (e.g., "C") will now abort on uncaught unwinds". So any tantivy panic that is not caught with `catch_unwind` at the FFI boundary kills the whole Go process. An abort, a segfault or an OOM in native code does the same in-process.
- Several Rust `staticlib`s in one binary collide on duplicate std symbols.
  - The Android build team changed Soong to stop linking Rust staticlibs directly for this reason: https://gerrit.omnirom.org/plugins/gitiles/android_build_soong/+/0a468a4f3b1c9dea9b31ca51cebd3db58d65e771%5E%21/cc. **B** (the platform build system's own commit).
  - tantivy-go links with `-Wl,--allow-multiple-definition` on Linux (its `tantivy_remote.go`, **A**).
  - **For codectx this applies only if a second Rust staticlib is ever linked.** Its other native code, tree-sitter, is C.

### 4.3 Per-document marshalling (what matters for a complete-answer collector)

- **tantivy-go (Anytype)**, https://github.com/anyproto/tantivy-go, v1.0.6, 2026-01-06. **A** for what the code does; **B** for the production claim.
  - It builds a Rust `crate-type = ["staticlib"]` against a pinned tantivy fork, `anyproto/tantivy` at a fixed rev.
  - Results are read **one cgo call per hit**, `search_result_get_doc(ptr, index)`, followed by per-field `CString` copies and a JSON conversion (`searchresult.go`, `document.go`).
  - The README says it has run "in Anytype … for over a year across all major platforms". `anytype-heart`'s `go.mod` pins `github.com/anyproto/tantivy-go v1.0.6`.
- **Milvus**, https://github.com/milvus-io/milvus (Go). **A** (source).
  - `internal/core/thirdparty/tantivy/tantivy-binding/Cargo.toml` has `crate-type = ["staticlib", "rlib"]`, `lto = true` and `panic = "unwind"`, with `cbindgen` headers. Its tantivy is a fork, `zilliztech/tantivy`.
  - The library is linked into the C++ segcore, which Go reaches through `#cgo pkg-config: milvus_core` (`internal/util/cgo/executor.go`).
  - Search and load run on **bounded native thread pools**, sized from config (`executor_set_search_thread_num`). This is the CockroachDB advice in production form: coarse calls and bounded concurrency.
- **Estimate (arithmetic, not a measurement).** A 50 000-hit complete answer:
  - One cgo crossing per hit costs ≥ 50 000 × ~30 ns ≈ 1.5 ms in crossings alone, before any per-field copy or allocation.
  - One call that returns a packed array of (document key, f64 score) copies 50 000 × 16 B ≈ 0.8 MB once.
  - Design rule this implies: **one crossing per query or page, never per hit.** The per-hit pattern in tantivy-go is the cautionary example.
- **Sidecar process.** *ipc-bench*, Peter Goldsborough, https://github.com/goldsborough/ipc-bench. **C+**: reproducible code, an old i5-4590S, an undated run.
  - Ping-pong throughput with 100-byte messages: pipes 162 441 msg/s (about 6 µs per round trip), Unix domain sockets 130 372 msg/s, shared memory 4.7 M msg/s.
  - A sidecar therefore adds microseconds per query, not per hit, provided results are streamed as packed pages. In exchange it isolates panics, aborts and native memory from the Go process.
  - Codectx already uses this pattern for its native parsers: a worker mode of the same binary, with the parent side owning no native object (codectx docs, `providers-treesitter.md`).

**Credibility of §4 overall.** Per-call costs are consistent across three independent microbenchmarks and the Go team's own release notes. Nobody has measured the per-hit cost of a whole-answer collector across cgo or a pipe; it must be measured in the harness.

---

## Evidence is thin where

- **The only reproducible multi-engine benchmark** is one-field, read-only and query-only. It reports no indexing throughput, disk, memory, deletes or multi-field scoring, and the tantivy team runs it.
- **No reproducible comparison exists** for Bleve, Bluge, Xapian, Zoekt or Meilisearch against tantivy.
- **The FTS5 comparison** is one person, one run, versions unstated (C).
- **No benchmark uses a code corpus.** Nothing measures a complete-answer collector (every hit scored and ordered) on any engine, or cgo against sidecar cost for whole-answer transfer.

## Is any engine likely to suit a local code-search product better than tantivy?

**No, on the available evidence, for a BM25-ranked lexical tier that must give complete, exact and deterministic answers.**

- **Lucene** is as fast or faster on top-k, but it brings a JVM into a single Go binary.
- **Xapian** is GPL v2+ and keeps at most two revisions, which contradicts rule 5.
- **Meilisearch** is a server without BM25, capped at 1 000 hits by default, which contradicts rule 3.
- **PISA** is an in-memory research engine whose impacts need a single segment.
- **Bluge** has been unmaintained since 2022 and carries an idf precedence defect.
- **Zoekt** is a different class (substring and regex). It is the right shape for a grep-like tier, not a replacement for BM25 with idf.
- **iResearch** is fast in the benchmark, but its standalone repository is archived and it now ships inside SereneDB.
- **Bleve** is the one credible pure-Go alternative: maintained, Apache-2.0, and it removes the native dependency. But it scores per field, and there is no published performance evidence. It is worth a benchmark arm only if avoiding a second native dependency is weighted heavily.
- **The incumbent (FTS5 plus packed segments)** remains a valid baseline. It is exact, transactional and pure Go. Its scoring is already BM25F-shaped (weighted tf, one saturation), so codectx's model is close to FTS5's `bm25()` apart from the idf form. Its cost is the per-posting overhead that codectx has measured itself.

## What this means for codectx

| rule | verdict | why |
|---|---|---|
| rule 1 exact df over the column union, visible docs only | **neutral** | No surveyed engine computes a union df over visible documents. FTS5's nHit is the union over columns, counted by running the phrase query over the current table (`xQueryPhrase`), so it reflects live rows at the cost of a phrase scan per term (`fts5_aux.c`). Bleve and tantivy compute it per field, and Zoekt drops idf entirely. |
| rule 2 delta = fresh | **neutral** | No engine documents this property. ParadeDB and Turso both control segment lists and merges themselves to get transactional behaviour. |
| rule 3 no top-k cap | **supports keeping it; contradicts Meilisearch** | Meilisearch defaults to 1 000 hits and GitHub returns a top 100. In the benchmark, tantivy is faster than Lucene on `TOP_100_COUNT`, the exhaustive mode. |
| rule 4 content-only deterministic ties | **neutral** | Not benchmarked anywhere. |
| rule 5 retained snapshots | **supports owning it outside the engine** | ParadeDB keeps per-segment MVCC visibility outside tantivy, which is production precedent for that mechanism. Xapian's two-revision limit contradicts the rule. Lucene alone ships retained commits natively (`IndexDeletionPolicy`); tantivy has no equivalent (see the crate notes). |
| rule 6 soft deletes survive merges | **supports owning merges** | Turso runs `NoMergePolicy` and ParadeDB serialises merges by transaction id, both production precedents for the product choosing when to merge. |
| rule 7 f64 BM25F, one saturation | **supports** | FTS5's `bm25()` and Zoekt's BM25 both fold weighted fields into one tf before one saturation. tantivy, Bleve and Bluge score per field. So BM25F is the norm in the two code-relevant and embedded engines, not an oddity; see 01 for the quality evidence. |
| rule 8 FTS5 unicode61 tokenizer | **neutral** | Engine choice does not force it; tantivy and Bleve both accept custom analysers. |
| rule 9 tier first, then lexical score | **neutral** | Zoekt's and GitHub's rankers mix filename and symbol signals into the score, not into tiers; see 08. |
| rule 10 keyset-cursor paging | **neutral** | No engine surveyed offers content-keyed cursors natively. Meilisearch's offset is capped. |
| rule 11 1e-6 score quantization | **neutral** | Not addressed by any source. |
| Single Go binary | **qualified support for tantivy** | cgo with a Rust `staticlib` is proven in production (Milvus, Anytype). codectx already requires cgo for tree-sitter, so the "no cgo" portability property is already gone. |
| Never freezes the host | **contradicts naive in-process use** | A panic aborts the process (Rust ≥ 1.81), and blocking cgo calls hold OS threads. Bounded native pools (Milvus), `catch_unwind` at every export, or the existing worker-process pattern are needed. |
| Complete answers | **design constraint** | One FFI crossing per query or page with packed results, never per hit (the tantivy-go pattern). Measure cgo against a sidecar on whole answers in the harness. |
