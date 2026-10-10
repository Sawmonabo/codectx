# 15 — Search runtime: executor, reader, reload policy, warmers, searcher reuse, explain

Version: tantivy 0.26.2. Paths are inside `tantivy-0.26.2/`. Docs: https://docs.rs/tantivy/0.26.2/tantivy/struct.Searcher.html, https://docs.rs/tantivy/0.26.2/tantivy/struct.IndexReaderBuilder.html, https://docs.rs/tantivy/0.26.2/tantivy/enum.ReloadPolicy.html, https://docs.rs/tantivy/0.26.2/tantivy/trait.Warmer.html

## Executor (intra-query parallelism)
- "By default the executor is single thread, and simply runs in the calling thread" (`src/index/index.rs:289-297`). `Index::set_multithread_executor(n)` (`:301`), `set_default_multithread_executor()` (all cores, `:313`), `set_executor(Executor)` (`:307`, a shared pool across indexes since 0.22). `Executor::single_thread()`, `Executor::multi_thread(n, prefix)`, `map`, `spawn_blocking` (`src/core/executor.rs:27-108`).
- `Searcher::search_with_executor`: "multithreading is by splitting search into as many task as there are segments. It is powerless at making search faster if your index consists in one large segment. Also, keep in mind multithreading a single query on several threads will not improve your throughput. It can actually hurt it. It will however, decrease the average response time." (`src/core/searcher.rs:210-232`).

## Reader and reload
- `ReloadPolicy::Manual` or `OnCommitWithDelay` (the default) (`src/reader/mod.rs:21-31, 53`). The automatic reload watches `meta.json`; the mmap directory's watcher polls every 500 ms (`src/directory/mmap_directory/file_watcher.rs:12`), and "This automatic reload can take 10s of milliseconds to kick in" (`src/reader/mod.rs:281-286`).
- `IndexReaderBuilder`: `reload_policy` (`src/reader/mod.rs:109`), `doc_store_cache_num_blocks` (`:118`, default 100), `warmers(Vec<Weak<dyn Warmer>>)` (`:128`), `num_warming_threads` (`:138`, default 1, `:56`), `try_into()` (`:66`).
- `IndexReader::searcher()`: "This method should be called every single time a search query is performed. The same searcher must be used for a given query, as it ensures the use of a consistent segment set." (`src/reader/mod.rs:291-298`). `reload()` (`:287`).
- A `Searcher` is an immutable snapshot: "regardless of commits, file garbage collection, or segment merge that might happen, as long as the user holds and reuse the same Searcher, search will happen on an immutable snapshot of the index" (ARCHITECTURE.md, "Searcher"). `Searcher::generation()` exposes a `SearcherGeneration` with a `generation_id` and the map SegmentId -> delete opstamp (`src/core/searcher.rs:54-80`).
- Opening is cheap: "loading an index is as fast as mmapping its files" (ARCHITECTURE.md, "General information"); README: "Tiny startup time (<10ms), perfect for command-line tools".

## Warmers
`trait Warmer { fn warm(&self, &Searcher) -> Result<()>; fn garbage_collect(&self, live: &[&SearcherGeneration]); }` (`src/reader/warming.rs:14-20`). It runs on each new searcher generation before it is served. `examples/warmer.rs` caches a per-segment column keyed by `(SegmentId, delete_opstamp)` and drops entries for dead generations. The natural codectx use is a per-segment cache of alive statistics (alive N, alive token total, or alive df for hot terms), computed once per generation instead of once per query.

## Explain
`Query::explain(&searcher, DocAddress) -> Explanation` (`src/query/query.rs:138-142`); `Explanation::to_pretty_json()` (`src/query/explanation.rs:79`). The probe's explain output (see 13) shows idf, n, N, freq, k1, b, dl, avgdl. Useful as a debugging tool, not a hot-path feature.

## Async
`Searcher::doc_async` and `doc_freq_async` exist only with the `quickwit` feature (`src/core/searcher.rs:106-117, 146`).

## Snippets and highlighting
`SnippetGenerator::create(&searcher, &query, field)` (`src/snippet/mod.rs:400`) collects the query's terms for that field; `set_max_num_chars(n)` (default 150, `:70, :435`); `snippet(text)` works on any text the caller supplies, `snippet_from_doc(doc)` on a stored document (`:448-466`). A `Snippet` gives `fragment()`, `highlighted()` byte ranges, `to_html()` with `<b>` by default, and `set_snippet_prefix_postfix` (`:135-178`; `examples/snippet.rs`). It re-tokenizes the text with the field's analyzer, so it highlights exactly the terms the index matched. The bench measures ranking and hits, not result previews, so this is "no" for the bench. If codectx ever wants highlighted previews, the text must come from codectx's own store, because `body` is not stored in the bench index.

## What it means for codectx
- codectx's CLI opens an index per command, so the relevant numbers are open + first query (cold) and warm queries on a reused `Searcher`. the bench's Rust arm already times cold in a fresh process and warm on a reused searcher, with `ReloadPolicy::Manual`.
- The executor helps only with many segments; it competes with the product rule "never freezes the host" when many queries run at once. Variant: `--search-threads {1, 4, 8}` on a many-segment index (unit cadence) and on a one-segment index; compare warm latency and CPU time.
- Warmer variant: compute the product's corpus statistics (N and token total from `token_count`) in a `Warmer` and compare product-mode cold latency with the current per-query scan.
- `OnCommitWithDelay` is irrelevant to a CLI; it matters only to a long-running server process.
