# Incremental updates: synthesis and recommendation (2026-09-13)

Inputs: reports 11 (engine, measured), 12 (SCIP indexers and LSP servers, measured), 13 (peer
strategies, sourced). Question: after the first full index, does a local edit or a commit re-run
everything, including the dependence engine, and how do we make it incremental?

## 1. What happens today (plan as written)
| Layer | On one edited file | On a commit / branch switch |
|---|---|---|
| filesystem, docs, tree-sitter | that file only | changed files only; unchanged units reused by hash |
| SCIP indexer | whole declared unit re-indexed and **fully re-imported** | every unit with any changed file |
| dependence engine | whole frontend-native unit parsed + exported + fully re-imported; C/C++ = whole repo | same |
| LSP overlay | already live and incremental for pinned files | n/a |

Measured price of the unit re-run for one edited file: 240k-line Go module 16–28 s engine wall;
500k-line Python tree 53–65 s; SCIP: 0.3–1 s Go (warm build cache), 6–9 s TypeScript (191k),
83–87 s Python (mypy, 1,316 files). Measured change caused by that edit: ~1 in 10,000 fact rows
(engine) and 1 of 141 / 954 / 76 / 1,316 documents (SCIP). Almost all the re-run is wasted work.

## 2. What the third-party tools offer (verified)
- **Engine:** nothing. Maintainer: "No, it doesn't right now" (joern#5757). No merge, no append,
  no per-file export; `--overlaysonly` re-application corrupts graphs. File-only parses reproduce
  intraprocedural control/data dependence exactly for Python and TypeScript, approximately for Go
  (package `<clinit>` artifacts), and lose cross-file call names (TypeScript badly: 75%, wrong names).
- **SCIP indexers:** scip-java is per-file by design (compiler plugin shards; nothing evicts stale
  shards). scip-go and scip-clang accept narrowed inputs with exact documents (one named exception
  each). scip-typescript, scip-python, rust-analyzer must run whole. The **format** permits
  document-level replacement (documents independent, symbols position-free, locals document-scoped);
  it has no recency marker, so replacement = delete-then-insert keyed by path, set-based deletes.
- **LSP servers:** all incremental internally; none exports facts (rust-analyzer's `scip` aside).
- **Peers:** no one is incremental *inside* a precise analyzer unless designed for it (Infer
  summaries, Glean ownership). Everyone gets it *around* the analyzer: content-addressed input
  closures, early cutoff on declaration-only digests, stale-with-distance serving, two-tier freshness.

## 3. Recommendation (ranked, all independent of tool cooperation)
1. **Delta import for both semantic providers (Task 9 + Task 11).** Keep whole-unit runs and the
   cpg cache; replace the rewrite with a diff against the stored unit: SCIP by document (hash the
   canonical document, delete-then-insert changed paths, delete stored paths absent from the fresh
   index, reject documents outside project root), engine by id-independent fact key (normalize Go
   `<clinit>` nondeterminism). Cost: ~1–5 s of keying, against 17–65 s engine runs. Collapses the
   storage/FTS/reconcile cost to ~0.01–2% of today's.
2. **Retention by distinct ref, not by snapshot count (Task 12/20, §12.4, §20).** Keep the results of
   the last `retain_refs` branches/commits the user indexed (default 8) so A→B→C→A reuses A's units.
   No default size limit; `max_retained_bytes` is user-set only (same posture as memory). User ruling
   2026-09-13: a forced size cap was rejected.
3. **Carried-over stale units with a labelled distance (§13.3, Task 12).** During the 20–60 s
   refresh, dependence answers stay available and marked `stale` (generation distance) instead of
   `pending`. Honesty rules already exist in the capability vocabulary.
4. **Early cutoff on a declaration-only signature digest (new task after Task 12).** A body-only
   edit must not invalidate the SCIP or dependence unit of a 240k-line module. Highest value for
   local edits, highest risk (silent wrong answers if the digest is wrong); per-language proof gate.
5. **Per-file fast-preview tier for Python only (optional, later).** Exact intraprocedural
   control/data dependence and reads/writes from a single-file parse, published at reduced
   precision while the whole unit refreshes; never publish its `calls`. Not Go, not TypeScript.
6. **Java true incrementality via scip-java shards, with our own shard eviction (later).**
7. Keep: unit sizing (per-package Python already cuts edit latency from ~60 s to ~4 s), LSP-first
   freshness for unsaved/live edits, Go build cache warm (0.3 s vs 10 s).

Rejected: naive SCIP concatenation, narrowed scip-typescript/scip-python, engine graph splicing
on internals, stack-graphs-style per-language resolvers, waiting for upstream.
