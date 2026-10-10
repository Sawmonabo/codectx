# 05 — Every place the tantivy arm departs from our engine

Product mode is the arm meant to equal our engine. On the smoke fixture, a reference implementation with no index (`tcheck`) agreed with product mode on hits and on the first page for 18 of 18 queries. It agreed on single-segment and multi-segment indexes, with kind and path filters, and on 19 of 19 queries after a delta. The departures below are everything that remains.

**All modes**
1. **Lexical tier only.** Our search also runs the exact name, qualified-name and prefix tiers, ranks tiers before score, and folds across tiers. None of that exists here. The arm also omits `max_query_terms`, the truncation reason and range hydration.
2. **Over-long tokens.** When FTS5's 32 768-byte cut falls inside a character, SQLite keeps the partial bytes. Here the cut is at the character boundary, because a tantivy term is a `String`. Matching is the same; that term's bytes differ (01-tokenizer-probe.md).
3. **Extra fast fields.** `node_id`, `name_s`, `qn_s`, `sig_s`, `start` and `end` exist only for the fold and the comparator, and they count in disk bytes. `body` is indexed with positions but not stored. Our store keeps no body either.
4. **Cold runs.** Cold means a fresh process with the OS page cache left as it is. Index open is untimed.
5. **Commits.** A per-unit commit is a tantivy segment flush, not a SQLite seal transaction (04-commit-cadence.md). The delta uses 1 indexing thread and a 50 MB budget.

**Product mode**
6. Product cold runs include one scan of the `token_count` fast field over live documents, to get N and the token total. Our engine reads an aggregate stored at activation. The warm process does the scan once, untimed.

**Library and library-all**
7. **Different scoring by design.** tantivy's BM25 computes df per (field, term). Its df and document count include deleted documents (`tantivy-0.26.2/src/core/searcher.rs:133-141`, `src/query/bm25.rs:38-44`), so after a delta its statistics drift from the live set. It uses quantized fieldnorms, not the exact `token_count`, and f32 scores.
8. **No fold.** `hits` are raw documents. A file whose body splits into several chunks counts once in our engine and once per chunk here. Smoke: "the" has 1 799 raw documents against 994 folded entities.
9. **Phrases are uncapped.** Our engine counts phrase adjacency over the first 512 positions per (document, column, token). tantivy's PhraseQuery reads every position. Smoke: `"the the"` gives 78 here against 76.
10. **Ties.** library ties fall to document address, library-all ties to `id`. This is why the two can differ slightly on the same BM25.
11. **The scorer paths differ in the last f32 bit.** Within the library, the pruned path (TopDocs, block-WAND) and the exhaustive path (every document scored) can give the same document scores that differ by 1–2 ulp, because the f32 sums are taken in different orders. On the smoke fixture this affected 11 page scores on a 6-segment index and 80 after a delta, and never changed a page (08-topk-check.md). Our engine quantizes an f64 score and has one path only.
12. **The code tokenizer variant** (`tindex --tokenizer code`) is deliberately not our tokenizer. It also emits camelCase parts at the position of their whole token. That inflates fieldnorms and lets a phrase match across adjacent identifiers. Product mode refuses such an index.
13. **Multi-threaded search** (`--search-threads`) is offered for the library modes only. Our engine answers each query on one thread.

**Contract.** Result lines are the frozen contract. The optional `filter` and `search_threads` keys appear only on runs that use those options.
