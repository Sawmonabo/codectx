# 03 — The three query modes: construction and semantics

All modes parse queries the same way, as our `parseQuery`/`splitQuoted` do. Text is split on `"`. Each quoted run is tokenized into one phrase term; each unquoted run is tokenized into single-token terms. Terms are folded by key, keeping the first occurrence, and empty keys are dropped. The candidate set is the OR of the terms, as `encodeFTS` joins them. Every mode serves pages of 200.

**library** (what a typical application does)
- Query: `BooleanQuery` SHOULD over the terms. Each term is a SHOULD over the five text fields of `BoostQuery(TermQuery | PhraseQuery, 5/5/2/2/1)`.
- Collector: `TopDocs::with_limit(200).order_by_score()` (`tantivy-0.26.2/src/collector/top_score_collector.rs:93,226`).
- Scoring: tantivy's own BM25: per-field statistics, fieldnorms quantized to 256 buckets (`src/query/bm25.rs:62-66`), f32 scores. The library may prune with block-WAND (`src/query/boolean_query/boolean_weight.rs:516-525`).
- Timed: the search plus the 200 stored-document fetches for `id`.
- `hits`: a separate, untimed `Count` of raw documents.
- Ties: the library's order (document address).

**library-all** (the library's ranking under our "no top-k" rule)
- Same query and BM25. A custom collector with `requires_scoring() = true` keeps every `(doc, score)` from every segment, so nothing is pruned.
- The set is ordered by score descending, then by `id` ascending; the first 200 are served.
- To break ties, it reads `id` from the fast field only for documents scoring at least the 200th best score. All other candidates are only compared on score.
- `hits` = the number of raw documents collected, inside the timed part.
- The custom collector was chosen over `TopDocs(limit = Count)` because the latter needs a counting pass first and then a heap the size of the whole match set.

**product** (our engine's rules on tantivy's index): see 06-extension-points.md.
- Exact BM25F, with df, N and token totals over live documents.
- The fold by node id, else by (path, start byte).
- Our total order, cmpScored.
- `hits` = folded entities. The raw document count is printed on stderr.

**Options (all modes unless stated)**
- `--kind K` (repeatable): the kind must equal one of the K given, as `matchesKinds` requires.
- `--path-prefix P` (repeatable): whole folders, our filter's rule. The path must equal `P` or start with `P/`, after the same cleaning (trimmed, `\` → `/`, trailing `/` dropped). Prefix `doc` keeps `doc/x.go` and not `docs/x.go`; an exact file path matches itself. The smoke checked all three modes: `doc` → 0 hits, `docs` and `docs/` → same counts, an exact file → 12 hits in every mode.
- How library modes filter: a MUST `ConstScoreQuery(…, 0.0)` (`src/query/const_score_query.rs:19`) over a TermQuery on `kind` and on `path_s` (equality), and over `RegexQuery::from_pattern(escape(P) + "/.*", path_s)` (`src/query/regex_query.rs:64`). The filter adds no score.
- How product mode filters: in its collector, after scoring and before the fold, as `lexicalCandidates` does. df and the corpus statistics stay unfiltered.
- Recording: lines carry `"filter"` (for example `kind=function|method;path=pkg/api`). Their phase becomes `…-filtered`.
- `--search-threads N`: `Index::set_multithread_executor(N)` (`src/index/index.rs:301`), which searches segments in parallel. Library modes only; product mode refuses it, because our engine answers on one thread. Lines carry `"search_threads"`, and the phase gains `-sN`.
- `--page N`:
  - library serves page N from the library's own top-k, `TopDocs::with_limit(200).and_offset((N-1)·200)` (`tantivy-0.26.2/src/collector/top_score_collector.rs:139`), so page N costs a top-(N·200) and is timed like page 1;
  - library-all serves the same slice of its one full collection, resolving ids only for documents at or above the (N·200)-th score;
  - product refuses the option;
  - lines carry `"page_no"`, and the phase gains `-pN`.
- `--explain K` (library only): after the timed runs, and untimed, the library's `Query::explain` (`src/query/query.rs:138`) of the first K hits of the served page is written as JSON (`Explanation::to_pretty_json`, `src/query/explanation.rs:79`) to `results/<arm>-<phase>-explain.jsonl`, one `{query, rank, id, score, explanation}` per hit, so per-field and per-clause contributions are visible.
- `--phase NAME` overrides the phase.
- The optional keys are omitted on unfiltered, single-thread runs, so those lines stay exactly the frozen contract.

**Cold and warm runs.** Run 0 of each query runs in a fresh child process. The index open is untimed; the OS page cache is not dropped. Warm runs repeat in the parent process, and a warm answer that differs from the cold one is an error.
