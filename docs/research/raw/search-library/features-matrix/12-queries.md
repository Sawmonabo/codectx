# 12 — Queries, the query parser, and one verified scoring defect

Version: tantivy 0.26.2. Paths are inside `tantivy-0.26.2/`. Docs: https://docs.rs/tantivy/0.26.2/tantivy/query/index.html

## Query types (exported at `src/query/mod.rs:37-71`)
| Query | API | Scoring | Source |
|---|---|---|---|
| Term | `TermQuery::new(Term, IndexRecordOption)` | BM25 if the option reads freqs | `src/query/term_query/term_weight.rs` |
| Term set | `TermSetQuery::new(terms)` | constant: built on `AutomatonWeight`, whose scorer is a `ConstScorer` | `src/query/set_query.rs:19, 59`; `src/query/automaton_weight.rs:109` |
| Boolean | `BooleanQuery::new(Vec<(Occur, Box<dyn Query>)>)`, `union`, `intersection`, `new_multiterms_query`, `with_minimum_required_clauses`, `set_minimum_number_should_match` | sum of clause scores | `src/query/boolean_query/boolean_query.rs:180-257` |
| Phrase | `PhraseQuery::new`, `new_with_offset`, `new_with_offset_and_slop`, `set_slop` | BM25 with idf = SUM of the terms' idfs and tf = phrase count | `src/query/phrase_query/phrase_query.rs:35-90, 124`; `src/query/bm25.rs:120-128`; `phrase_scorer.rs:578-585` |
| Phrase prefix | `PhrasePrefixQuery::new`, `set_max_expansions` (default 50) | BM25 | `src/query/phrase_prefix_query/phrase_prefix_query.rs:8, 39-68` |
| Regex phrase | `RegexPhraseQuery` | BM25 | `src/query/mod.rs:57`; CHANGELOG.md:134-135 |
| Regex | `RegexQuery::from_pattern(pat, field)` | constant | `src/query/regex_query.rs:64` |
| Fuzzy | `FuzzyTermQuery::new(term, distance, transposition_cost_one)`, `new_prefix` | constant ("the score is not lower for the fuzzy hit", `examples/fuzzy_search.rs:153`) | `src/query/fuzzy_query.rs:93-103` |
| Range | `RangeQuery::new(Bound<Term>, Bound<Term>)` (uses the fast field when present), `FastFieldRangeQuery`, `InvertedIndexRangeQuery::limit` | constant | `src/query/range_query/range_query.rs:80, 134-144`; `range_query_fastfield.rs:28` |
| Exists | `ExistsQuery::new(field, json_subpaths)` | constant | `src/query/exist_query.rs:64` |
| Disjunction max | `DisjunctionMaxQuery::new`, `with_tie_breaker` | max + tie_breaker x rest (but see the defect below) | `src/query/disjunction_max_query.rs:92-105`; `src/query/score_combiner.rs:62-95` |
| Boost | `BoostQuery::new(q, f32)` | child score x boost; passes the boost INTO the child scorer | `src/query/boost_query.rs:70-71` |
| Const score | `ConstScoreQuery::new(q, f32)` | constant; wraps the child scorer | `src/query/const_score_query.rs:66-69` |
| More like this | `MoreLikeThisQuery::builder().with_min_doc_frequency(..)...with_document(addr)` | BM25 over the chosen terms | `src/query/more_like_this/query.rs:41-178` |
| All / Empty / Exclude | `AllQuery`, `EmptyQuery`, `Exclude` | — | `src/query/mod.rs:39-48` |

`Query` trait helpers: `explain(searcher, DocAddress)` (`src/query/query.rs:138-142`), `count(searcher)` (`:145`), `query_terms(visitor)` (`:162`).

## QueryParser
API (`src/query/query_parser/query_parser.rs`): `for_index(index, default_fields)` (`:279`), `set_conjunction_by_default()` (`:288`), `set_field_boost(field, f32)` (`:299`), `set_field_fuzzy(field, prefix, distance, transposition_cost_one)` (`:310`), `allow_regexes()` (`:328`, new in 0.26, CHANGELOG.md:40), `parse_query` (`:336`), `parse_query_lenient` returning errors alongside a best-effort query (`:349`). Grammar (doc comment `:150-205`): AND/OR (AND binds tighter), `+`/`-`, quoted phrases with `~slop`, `"phrase pref"*`, ranges `[a TO c}`, `field: IN [a b]` (a term set), `*` for all docs, `^boost`. 0.26 also deduplicates repeated clauses (CHANGELOG.md:41).

Semantics that differ from codectx: a literal that tokenizes to more than one token becomes a phrase (`:958-995`), while codectx ORs the tokens of unquoted text (codectx `internal/search/lexical.go:297-308`). Default fields are combined as a SHOULD over fields, each field scored independently with its own statistics, i.e. per-field BM25 summed, not BM25F.

## Block-WAND applicability (when top-k pruning engages)
- A union is specialized to `TermUnion`, which runs block-WAND, only if every SHOULD scorer is a `TermScorer` reading frequencies: "Block wand is only available if we read frequencies" (`src/query/boolean_query/boolean_weight.rs:57-68`). `BooleanWeight::for_each_pruning` sends a `TermUnion` to `block_wand`, anything else to the generic pruning loop (`:516-531`).
- `BoostQuery` keeps a `TermScorer` a `TermScorer` (it forwards the boost, `boost_query.rs:70-71`), so `SHOULD[Boost(Term WithFreqs) for each (term, field)]` is eligible. A nested `BooleanQuery`, a `PhraseQuery`, or `ConstScoreQuery` is not, so the bench's Rust arm's library shape (a SHOULD of per-term BooleanQuerys) never runs block-WAND, and any query with a phrase never does.
- A single `TermQuery` uses `block_wand_single_scorer` (`src/query/term_query/term_weight.rs:118-137`).

## Verified defect: DisjunctionMaxQuery under TopDocs sums instead of taking the max
`DisjunctionMaxQuery::weight` returns a `BooleanWeight` with a `DisjunctionMaxCombiner` (`disjunction_max_query.rs:92-105`). When every disjunct is a frequency-reading `TermQuery`, the union becomes `TermUnion` and `for_each_pruning` calls `block_wand`, which adds the scorers' scores and never consults the combiner (`src/query/boolean_query/block_wand.rs:200-204`). The library's own doctest passes only because it uses `IndexRecordOption::Basic`, which skips this path (`disjunction_max_query.rs:44-51`).

Probe (`../features/probes/src/main.rs`, a crate on tantivy =0.26.2, 23 documents, one segment): `DisjunctionMaxQuery::with_tie_breaker([title:alpha, body:alpha], 0.0)`, collected by `TopDocs::with_limit(10).order_by_score()` and by a collector that keeps every `(doc, score)` (no pruning path). Output:
```
dismax tie=0 opt=Basic
  TopDocs: [(1, 2.810509), (2, 2.810509), (0, 2.2204733)]
  all-docs collector: [(0, 2.2204733), (1, 2.810509), (2, 2.810509)]
dismax tie=0 opt=WithFreqs
  TopDocs: [(0, 4.4409466), (1, 2.810509), (2, 2.810509)]
  all-docs collector: [(0, 2.2204733), (1, 2.810509), (2, 2.810509)]
```
With `WithFreqs`, doc 0 (matches both fields) scores 4.44 = 2 x 2.22 under TopDocs and moves from last to first; the non-pruning collector gives the correct max, 2.22. So DisMax + TopDocs is only correct with Basic term queries (no tf), or with a non-pruning collector. Its main body:
```rust
let dm = DisjunctionMaxQuery::with_tie_breaker(vec![
    Box::new(TermQuery::new(Term::from_field_text(title, "alpha"), opt)),
    Box::new(TermQuery::new(Term::from_field_text(body, "alpha"), opt))], 0.0);
let top = searcher.search(&dm, &TopDocs::with_limit(10).order_by_score())?;
let all = searcher.search(&dm, &All)?; // Collector keeping every (doc, score)
```

## What it means for codectx
- The product's matching (OR of terms; phrase = adjacent positions in one column) is expressible with TermQuery/PhraseQuery under a BooleanQuery SHOULD, but the parser is not: build queries by hand, as the bench's Rust arm does.
- Flat vs nested: a flat `SHOULD[Boost(Term)]` over (term, field) pairs is the only shape that gets block-WAND. Variant: flat vs nested on the term-only bands (phrase queries split out), TopDocs(200); compare warm latency and the page against the nested shape and the all-docs page.
- DisMax with tie_breaker is "maybe" for quality (closer to per-field max scoring), but it must be collected without pruning or with Basic options, because of the defect above. Variant: DisMax(tie 0.1, 0.3) per term over the five boosted fields, collected by an all-docs collector; compare page overlap with product mode.
- TermSet, Regex, Fuzzy, Range, Exists, ConstScore are constant-scoring filters. Fit: kind filter (TermSet or SHOULD of Basic terms), path prefix (`RegexQuery` `prefix/.*`, or a range on `path_s` from `p/` to `p0`). Variant: path filter as RegexQuery vs RangeQuery on the fast field; compare filtered latency. `PhrasePrefixQuery` (cap 50 expansions) and `FuzzyTermQuery` change answers and are out of scope for the product arm; MoreLikeThis is not a codectx query.
