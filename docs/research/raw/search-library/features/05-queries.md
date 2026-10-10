# 05: Query types in tantivy 0.26.2, and which of them a code-search product can use

**Sources.** The crate source is the reference: `tantivy-0.26.2`, `tantivy-query-grammar-0.26.0` and `tantivy-fst-0.5.0`, all from crates.io. Line numbers below were checked against those files. Docs cross-checked: https://docs.rs/tantivy/0.26.2/tantivy/query/struct.QueryParser.html shows the same eight method signatures as the source. https://docs.rs/tantivy/0.26.2/tantivy/query/struct.PhrasePrefixQuery.html does not give the default for `set_max_expansions`. The source sets it to 50 (below). The CHANGELOG lines quoted are from `tantivy-0.26.2/CHANGELOG.md`.

**The product's lexical model, restated so this file stands alone:**
- **Columns.** Each document has five text columns: `name`, `qualified_name`, `signature`, `path` and `body`, weighted 5/5/2/2/1. It also has `kind`, `path`, `id` and `token_count`.
- **Query text.** It is split on `"`. A quoted run becomes one phrase term. An unquoted run is tokenized into single-token terms. Terms are de-duplicated by key, and the candidate set is the OR of all terms.
- **Scoring.** BM25F: the column-weighted term frequency is summed before saturation. df counts the union of the five columns over live documents only. The length is `token_count`. Arithmetic is f64, then quantized to 1e-6.
- **Rules.** Answers are complete (no top-k cap), and the order is total and deterministic.
- **Filters.** Users filter by `kind` (equality, several allowed) and by folder. The folder rule matches the folder itself and everything under `P/`. So `doc` never matches `docs/`, and an exact file path matches itself.

**The benchmark harness** (index fields: five text fields with positions; `kind`, `path_s` and `id` as `STRING | STORED | FAST`; `token_count` as u64 `FAST`):
- `tquery <dir> <queries> <runs> library|library-all|product [--kind K]… [--path-prefix P]… [--search-threads N] [--phase NAME]`
  - `library`: tantivy BM25 with `TopDocs(200)`.
  - `library-all`: the same query, with a custom collector that keeps every (doc, score). It sorts by score, then `id`.
  - `product`: the product's own rules on the same index.
- `tqrels <docs> <outdir> <seed>` writes `queries-{name,doc,subword}.txt` and `qrels-*.tsv`.
- `teval <qrels> <queries> <results.jsonl>…` reports MRR@10, S@1, S@10, R@200, mean hits, zero-hit queries and missing queries.
- `tcompare a.jsonl b.jsonl` reports hit counts and top-200/top-10 overlap.
- Latency is the cold and warm median from `tquery` result lines. Disk bytes come from `tindex`.

When no existing mode exercises a capability, the variant below says **"needs a new mode"** and specifies it. It does not invent a flag.

---

## Cross-cutting facts that decide most rows

1. **Three query kinds score with BM25; the rest score a constant.**
   - BM25 is computed only by `TermQuery` (via `TermWeight` → `TermScorer`), `PhraseQuery` and `PhrasePrefixQuery`. All three build a `Bm25Weight` (`src/query/term_query/term_query.rs:104`, `src/query/phrase_query/phrase_query.rs:124`, `src/query/phrase_prefix_query/phrase_prefix_query.rs:115`).
   - The following match through a doc bitset with a constant score equal to the boost (1.0 by default): `RegexQuery`, `FuzzyTermQuery` and `TermSetQuery` (all use `AutomatonWeight`; `ConstScorer::new(doc_bitset, boost)` at `src/query/automaton_weight.rs:109`), `RangeQuery`, `ExistsQuery` and `AllQuery`.
2. **BM25 statistics come from `Bm25StatisticsProvider`, per field, and include deleted documents.**
   - `total_num_docs` sums `max_doc` (`src/query/bm25.rs:38-44`).
   - `Searcher::doc_freq` sums each segment's term-dictionary `doc_freq` (`src/core/searcher.rs:133-141`). That count includes deleted documents until a merge removes them.
   - `Score` is `f32`, and K1 = 1.2, B = 0.75 (`src/query/bm25.rs:8-9`).
   - No query type gives cross-column df, live-only df, or f64 scores. These are properties of the scoring layer, not of the query you pick.
3. **Block-max WAND applies only to a flat union of term scorers.**
   - It runs only when a `BooleanQuery`'s union consists entirely of `TermScorer`s that read frequencies (`src/query/boolean_query/boolean_weight.rs:57-66`), and only through `Weight::for_each_pruning`. That path is used by `TopDocs` ordered by score (`src/collector/sort_key/sort_by_score.rs:41,50`).
   - A union containing a nested `BooleanQuery`, a `PhraseQuery`, a `ConstScoreQuery` or an automaton query falls back to `BufferedUnionScorer`, which is exhaustive.
   - Example: the harness `library` query is SHOULD(term) of SHOULD(field) of `BoostQuery(TermQuery|PhraseQuery)`. Its outer union holds boxed boolean scorers, so by this reading it never takes the block-WAND path. A flat run (variant in §BooleanQuery) would confirm it.
   - The product's no-top-k rule needs every match anyway (`library-all`), so WAND only matters for the `library` comparison arm.
4. **Deleted documents are filtered at collection, not in the queries.**
   - `AllScorer`, the automaton bitsets and the postings all iterate up to `max_doc`.
   - The collector path skips documents absent from the alive bitset (for example `src/collector/sort_key/sort_by_score.rs:38-46`).
   - `Weight::count` uses the alive bitset (`src/query/weight.rs:78-85`).

---

## QueryParser

- **What it is.** It turns a user string into a `Box<dyn Query>` through a nom grammar (`tantivy-query-grammar`) followed by a schema-aware conversion.
- **Exact API** (`src/query/query_parser/query_parser.rs`):
  - `QueryParser::new(schema: Schema, default_fields: Vec<Field>, tokenizer_manager: TokenizerManager) -> QueryParser` (:254)
  - `QueryParser::for_index(index: &Index, default_fields: Vec<Field>) -> QueryParser` (:279)
  - `set_conjunction_by_default(&mut self)` (:288)
  - `set_field_boost(&mut self, field: Field, boost: Score)` (:299)
  - `set_field_fuzzy(&mut self, field: Field, prefix: bool, distance: u8, transpose_cost_one: bool)` (:310)
  - `allow_regexes(&mut self)` (:328)
  - `parse_query(&self, &str) -> Result<Box<dyn Query>, QueryParserError>` (:336)
  - `parse_query_lenient(&self, &str) -> (Box<dyn Query>, Vec<QueryParserError>)` (:349)
  - `build_query_from_user_input_ast(UserInputAst)` (:359) and its lenient form (:373)
  - The grammar alone: `tantivy_query_grammar::parse_query` and `parse_query_lenient` (`tantivy-query-grammar-0.26.0/src/lib.rs`).

### Behaviour

**Default fields.**
- A literal with no `field:` prefix expands to one clause per default field (:729-738).
- Each per-field clause is wrapped in that field's parser boost. Several fields are joined with `Occur::Should` (:779-793).
- If any field succeeds, errors from the others are dropped (:783-788): "it probably means the default_fields contains text and non-text fields".
- The result is plain BM25 per field, summed. It is not BM25F.

**Conjunction mode.**
- `default_occur()` returns `Must` when the mode is set and `Should` otherwise (:630-635). It applies only to clauses without an explicit `+`, `-`, `AND` or `OR` (:667).
- The doc comment (:136-141) says the default is a disjunction: "`happy tax payer` is equivalent to the query `happy OR tax OR payer`".

**Field boosts.** `field_boost` defaults to 1.0 (:689-690). A query-level `^b` multiplies with it (doc at :294-298). A boost of exactly 1.0 is dropped (`query_grammar.rs:841-850`), and negative boosts are rejected by the grammar ("Negative boosts are not allowed").

**Fuzzy settings.**
- With `set_field_fuzzy`, every single-term literal on that field becomes `FuzzyTermQuery::new` or `new_prefix` instead of `TermQuery::new(term, WithFreqs)` (`convert_literal_to_query`, :914-936).
- Those terms then score a constant, not BM25 (cross-cutting fact 1).

**Regexes.** They are off by default. `/re/` without `allow_regexes()` gives `UnsupportedQuery("Regex queries are not allowed.")` (:866-872), and a regex needs an explicit field (:876).

**Grammar** (`tantivy-query-grammar-0.26.0/src/query_grammar.rs`):
- **Words.** A word is any run of non-whitespace characters outside `^ \` : { } " ' [ ] ( ) \\`, and it may not start with `-` (:46, :72-91).
- **Reserved words.** `OR`, `AND`, `NOT` and `IN` cannot be words (:85).
- **Field prefix.** `name:` makes the next term field-scoped. A field name may not contain `+ ^ \` : { } " ' [ ] ( ) ! \\ * space` (:23-24).
- **Phrases.** `"…"` or `'…'` with `\` escapes (:154-185). They are followed by optional `~N` slop or `*` for a phrase prefix (:439-450).
- **Other leaves.** `IN [a b]` is a set (:633-648). `/re/` is a regex (:699-718). `[a TO b}`, `>5` and `<=10` are ranges (:450-453). `*` alone matches every document. `field:*` means exists.
- **Operators.** `+` and `-` (:866-871); `AND` and `OR`, where `AND` binds tighter (:895-1036); `NOT x` (:766-783); parentheses; `field:( … )` groups (:278-290).
- **Duplicates.** Duplicate clauses are removed (`rewrite_ast`, :1109-1124; CHANGELOG 0.26: "Deduplicate queries in query parser").

**Tokenizing a literal.**
- One literal goes through the field's tokenizer. One token gives `TermQuery`. **Two or more tokens give a `PhraseQuery`**, or the error `FieldDoesNotHavePositionsIndexed` (`generate_literals_for_str`, :973-995).
- So the unquoted literal `foo_bar` (or `foo-bar`, `a.b`) becomes the phrase "foo bar" whenever the tokenizer splits it.

**Negation and empty queries.**
- An all-negative query is an error (`AllButQueryForbidden`, :423) in strict mode. In lenient mode it is repaired by OR-ing `AllQuery` (:232-238).
- A query that trims to nothing becomes `EmptyQuery` (:1078).

**Exists in the parser.** `field:*` parses, but conversion rejects it with "Range query need to target a specific field." (:859-864). The message is wrong, and the parser cannot produce an `ExistsQuery`.

### Applies to codectx?

**Not as the parser for user text.** Three behaviours differ from the product's language:
1. Unquoted punctuated tokens become phrases. The product ORs the tokens: `foo_bar` matches `foo` or `bar`.
2. `:` is a field separator. Code queries such as `std::vec`, `pkg:func` or `http://` are parsed as field names and fail with `SyntaxError` or `FieldDoesNotExist` unless escaped.
3. `-x`, `+x`, `AND`, `OR`, `NOT`, `IN`, `^`, `*` and `/…/` are all operators, so ordinary code text changes meaning.

The product already parses its own text, and should build `BooleanQuery` trees directly, as the harness does. The parser stays useful only if the product ever wants an advanced syntax. Even then, default fields with field boosts give summed per-field BM25, not BM25F.

**Metric it would move.** Zero-hit queries and R@200 (failed parses or phrase-ified identifiers drop matches). Also MRR@10 and S@1 (summed per-field scores instead of BM25F).

**Variant.** Needs a new mode, `library-parser`:
- `QueryParser::for_index(&index, five text fields)`, with `set_field_boost` 5/5/2/2/1, disjunction, `parse_query_lenient` on the raw query line, and `TopDocs(200)`.
- Run it on `queries-name.txt`, `queries-doc.txt` and `queries-subword.txt` with `<runs>=5`.
- Then `teval qrels-*.tsv queries-*.txt` against `library`, and `tcompare` against `library` for hit-count deltas. Count the lenient errors per query.

---

## TermQuery

- **What it is.** It matches documents whose field contains one exact `Term` and scores them with BM25.
- **API.** `TermQuery::new(term: Term, segment_postings_options: IndexRecordOption) -> TermQuery` (`src/query/term_query/term_query.rs:73`). Also `term()` (:81) and `specialized_weight(EnableScoring) -> Result<TermWeight>` (:90).

### Behaviour

**Scoring.**
- With scoring on: `Bm25Weight::for_terms(statistics_provider, [term])` (:104), so the field's own df, N and average fieldnorm.
- With scoring off: a fixed weight of 1.0, and postings are read as `Basic` (:105-114).
- Stale doc comment: lines 14-19 still say the score is `idf * sqrt(term_freq / field norm)`. The code is BM25 (`TermScorer::score` → `Bm25Weight::score`, `src/query/term_query/term_scorer.rs:124-127`).

**Not-indexed fast field.** If the field is not indexed but is a fast field of a range-capable type, and scoring is off, the query runs as `RangeQuery[t, t]` over the fast column (:133-143). The code calls this "considerable slower". Added in 0.26 ("Add fast field fallback for `TermQuery` when the field is not indexed").

**Counting.** Without deletes, `count` reads `doc_freq` straight from the term dictionary. With deletes, it walks postings against the alive bitset (`src/query/term_query/term_weight.rs:56-65`).

**Saturated terms.** With scoring off, a term whose `doc_freq == max_doc` turns into `AllScorer` (`term_weight.rs:193-197`).

**Top-k.** `for_each_pruning` uses `block_wand_single_scorer` (`term_weight.rs:118-131`).

**Pitfall.** `IndexRecordOption::Basic` disables frequency reading, which also disables block-WAND in unions (`boolean_weight.rs:65`).

### Applies to codectx?

**Yes, as the building block for single-token terms per column.** It also serves as the `kind` and exact-`path` filter: use a `STRING` field with `IndexRecordOption::Basic`, inside a filter wrapper.
- Its score is per-field BM25 with deleted documents in df and N. It cannot be the product score, only the `library` comparison score.
- Product mode reads postings directly instead.

**Metric.** Warm and cold latency (the cheapest query). The filter path, through mean hits under `--kind`.

**Variant (existing).**
- `tquery <dir> queries-name.txt 5 library` and `… library-all`.
- Filtered: `tquery <dir> queries-doc.txt 5 library --kind function --kind method`. The kind filter is `TermQuery(kind, Basic)` under `ConstScoreQuery(…, 0.0)`.

---

## TermSetQuery

- **What it is.** It matches any of a set of terms (grouped per field) through an FST automaton, as an unscored union.
- **API.** `TermSetQuery::new<T: IntoIterator<Item = Term>>(terms: T) -> TermSetQuery` (`src/query/set_query.rs:19`). Terms are grouped by field, then sorted and deduplicated (:20-29).

### Behaviour

- Each field becomes a `Map` FST wrapped in `AutomatonWeight` (:48-61). The fields are combined in a `BooleanWeight` with `DoNothingCombiner` (:63-67).
- Every match scores `boost` (1.0) (`automaton_weight.rs:109`).
- Per segment, the scorer allocates a `BitSet` of `max_doc` bits and walks the dictionary with the automaton (`automaton_weight.rs:87-110`).
- A non-indexed field is a `SchemaError` (:41-45).
- Parser syntax: `field: IN [a b c]`.

### Applies to codectx?

**Yes, for the `kind` filter**, when a user passes several kinds: one `TermSetQuery` on `kind` instead of a SHOULD of `TermQuery`s.
- It is unscored, so it suits a filter. Wrap it in `ConstScoreQuery(…, 0.0)` or put it under `Occur::Must` inside a scored query; see the BooleanQuery notes on how MUST interacts with SHOULD.
- It cannot serve as a scored term union, because it would drop BM25.

**Metric.** Latency of filtered runs. Hits must be unchanged, which is the check.

**Variant.** Needs a new mode flag, `library-termset-filter`:
- Same as `library --kind function --kind method --kind type`, but the kind filter is `ConstScoreQuery(TermSetQuery([kind:function, kind:method, kind:type]), 0.0)`.
- Compare warm and cold medians, and `tcompare` hits (they must be identical).

---

## BooleanQuery (must / should / must-not, minimum_should_match)

- **What it is.** It combines sub-queries with an `Occur` and sums their scores.
- **API** (`src/query/boolean_query/boolean_query.rs`):
  - `BooleanQuery::new(Vec<(Occur, Box<dyn Query>)>)` (:180)
  - `with_minimum_required_clauses(subqueries, minimum_number_should_match: usize)` (:198)
  - `get_minimum_number_should_match` and `set_minimum_number_should_match` (:209, :214)
  - `intersection(Vec<Box<dyn Query>>)` (:219), `union(…)` (:225) and `union_with_minimum_required_clauses(…, usize)` (:231)
  - `new_multiterms_query(Vec<Term>)` (:244), which uses `TermQuery(WithFreqs)`
  - `clauses()` (:257) and `From<Vec<(Occur, Box<dyn Query>)>>` (:150)
  - `Occur::{Must, Should, MustNot}`

### Behaviour

**The default minimum.**
- `new` sets the minimum to 1 only when every clause is `Should`. If any clause is `Must` or `MustNot`, the minimum is 0 (:181-195).
- Consequence: `[(Must, filter), (Should, a), (Should, b)]` matches **every** document that passes the filter. The SHOULD clauses only add score (`RequiredOptionalScorer`, `boolean_weight.rs:329-337`).
- The text query must therefore be its own `Must` sub-`BooleanQuery`, or the minimum set to 1. The harness already nests it this way: text is one `Must`, each filter another `Must`.

**Matching and scoring.**
- MUST clauses are intersected. When the minimum is greater than 1, SHOULD clauses use `Disjunction` with that count (`boolean_weight.rs:22-41, 266-293`). MUST_NOT clauses are excluded through `Exclude` (:371-386).
- A lone `MustNot` clause matches nothing (:421-424).
- If the minimum exceeds the number of SHOULD clauses, the scorer is `EmptyScorer` (:261-264).
- The score is `SumCombiner`, the f32 sum of the matching clauses (`boolean_query.rs:163-168`; `score_combiner.rs:45-56`).

**Explain output.** It is "BooleanClause. sum of ..." (`boolean_weight.rs:449`).

**Shortcuts.**
- `AllScorer` and `EmptyScorer` children are folded away (`boolean_weight.rs:397-410`). CHANGELOG 0.26: "Optimize saturated posting lists by replacing them with `AllScorer` in boolean queries".
- Block-WAND applies only to an all-`TermScorer` union that reads freqs (cross-cutting fact 3).

### Applies to codectx?

**Yes. It is the query shape:**
- OR of terms → `Should`.
- Per term, the five columns → nested `Should` with `BoostQuery` 5/5/2/2/1.
- Filters → `Must`.

The `MustNot` and minimum-should features have no counterpart in the product language. A conjunctive mode (`minimum = number of terms`) is a possible product option, but not a current rule.

The sum over columns is per-field BM25 summed, not BM25F. BM25F needs the column-weighted tf before saturation, which only a custom scorer or collector can do (as product mode does).

**Metric.** MRR@10, S@1 and S@10 (sum vs BM25F). Latency of the flat vs nested shape (block-WAND eligibility, for `library` only).

**Variants.**
- Existing: `tquery <dir> queries-doc.txt 5 library`, `library-all` and `product`, then `teval`.
- Needs a new mode, `library-flat`: one flat `BooleanQuery` whose clauses are `(Should, BoostQuery(TermQuery(field, tok, WithFreqs), w))` for every (single token, field) pair. Phrases stay nested. Compare warm latency with `library` on `queries-name.txt` (single tokens dominate). Hits and order must match `library` up to f32 rounding (`tcompare` top-200 overlap).
- Needs a new mode, `library-msm`: `with_minimum_required_clauses(outer, n_terms)`, an AND of terms. Report R@200, zero-hit queries and mean hits against `library`.

---

## PhraseQuery (with slop)

- **What it is.** It matches an ordered sequence of terms in one field, at given relative positions, within a slop budget.
- **API** (`src/query/phrase_query/phrase_query.rs`):
  - `PhraseQuery::new(terms: Vec<Term>)` (:35)
  - `new_with_offset(Vec<(usize, Term)>)` (:43)
  - `new_with_offset_and_slop(Vec<(usize, Term)>, slop: u32)` (:48)
  - `set_slop(&mut self, u32)` (:80), `field()` and `phrase_terms()`

### Behaviour

**Constructor checks.** It panics (assert) with fewer than 2 terms or with terms from different fields (:49-58). It errors with `SchemaError` when the field has no positions (:107-118).

**Postings.** They are read `WithFreqsAndPositions` (`phrase_weight.rs:52-60`).

**Score.** `Bm25Weight.score(fieldnorm_id, phrase_count)` (`phrase_scorer.rs:578-586`):
- tf is the number of phrase matches in the document.
- idf is the **sum of the member terms' idf** (`src/query/bm25.rs:121-125`), not the phrase's own df.

**Slop.**
- The distance between consecutive terms is summed across the phrase ("carrying slop"), and transpositions are allowed (`phrase_scorer.rs:217-341`; CHANGELOG 0.21: "slop supports now transpositions").
- The accumulated slop is kept in a `u8` (`left_slops: Vec<u8>`, `phrase_scorer.rs:55`; casts `as u8` at :281, :298).
- Positions are not capped: every position is read.

**Related type.** `RegexPhraseQuery` (`phrase_query/regex_phrase_query.rs:46-98`) lets each phrase position be a regex or wildcard. Its default `max_expansions` is `1 << 14` (:73).

### Applies to codectx?

**Yes, for quoted phrases, one `PhraseQuery` per column.** It differs from the product in three ways:
- The product scores a phrase with the phrase's own df (its statistics key is the joined phrase). tantivy sums the term idfs.
- The product counts adjacency over the first 512 positions per (document, column, token). tantivy is uncapped. The harness smoke saw 78 vs 76 hits on `"the the"`.
- Slop has no product counterpart. The product phrase is exact adjacency, so slop must stay 0.

**Metric.** R@200 and mean hits on phrase queries (the 512 cap). MRR@10 (idf-sum vs phrase df).

**Variant (existing).** `tquery <dir> <queries-with-quoted-lines> 5 library|library-all|product`, then `tcompare library-all product` per phrase query. Hit deltas come from the 512 cap; order deltas come from idf. There is no slop variant, because slop > 0 is not a product rule.

---

## PhrasePrefixQuery

- **What it is.** It matches a phrase whose last term is a prefix, expanded against the term dictionary.
- **API** (`src/query/phrase_prefix_query/phrase_prefix_query.rs`): `PhrasePrefixQuery::new(Vec<Term>)`, `new_with_offset(Vec<(usize, Term)>)` (:47), `set_max_expansions(&mut self, u32)` (:67), `field()` and `phrase_terms()`.

### Behaviour

**Expansion limit.**
- `DEFAULT_MAX_EXPANSIONS: u32 = 50` (:8).
- The prefix expands, **per segment**, to at most `max_expansions` dictionary terms in byte order (`phrase_prefix_weight.rs:77-103`).
- Because the cut is per segment, which documents match depends on how the index is segmented.

**One-term case.**
- With a single term (`phrase_terms` empty), the query degrades to an `InvertedIndexRangeWeight` over `[prefix, prefix_end)` with `limit = Some(50)` (:139-157).
- That limit is enforced in the scorer regardless of features (`src/query/range_query/range_query.rs:222-228`). The scorer gives a constant score.
- Docs.rs says the constructor needs "at least two terms". The source accepts one (:48-51).

**Score.** BM25 with the idf sum of the **non-prefix** terms only (:112-116).

**Parser syntax.** `"a b"*`. One token gives the error `PhrasePrefixRequiresAtLeastTwoTerms` (`query_parser.rs:973-979`).

### Applies to codectx?

**Not under the product rules.** It has a default expansion cap of 50 and a per-segment truncation. Both break "complete answers", and the truncation also breaks "a delta index equals a fresh index". Setting `set_max_expansions(u32::MAX)` removes the cap but not the per-segment evaluation.

The product's own prefix tier is separate and outside this lexical comparison.

**Metric.** R@200 and mean hits (the truncation). Delta-vs-fresh identity of hits.

**Variant.** Needs a new mode, `library-phrase-prefix`:
- For each query, make the last token a prefix: `PhrasePrefixQuery` per column, `set_max_expansions(50)` and then `u32::MAX`.
- Run it on a multi-segment index and on the same corpus force-merged. `tcompare` the two: any hit delta shows the per-segment cap.

---

## RegexQuery

- **What it is.** It matches documents whose field contains any term that fully matches a regex, evaluated as a DFA over the term FST.
- **API** (`src/query/regex_query.rs`): `RegexQuery::from_pattern(regex_pattern: &str, field: Field) -> crate::Result<RegexQuery>` (:64) and `RegexQuery::from_regex<T: Into<Arc<Regex>>>(regex: T, field: Field)` (:71).

### Behaviour

**Matching semantics** (`tantivy-fst-0.5.0/src/regex/mod.rs:44-47`). The pattern is anchored at both ends: "every regular expression `(re)` is matched as if it were `^(re)$`". `\b`, `^`, `$` and lazy quantifiers are unsupported (:27-33).

**Limits.** The compiled program may be at most 10 MiB (`regex/mod.rs:81`) and the DFA at most 1 000 states (`regex/dfa.rs:8`). Beyond that, the pattern fails to build.

**Scoring and memory.** The score is constant (the `AutomatonWeight` bitset, `automaton_weight.rs:87-110`), and the weight ignores `EnableScoring` (`regex_query.rs:84`). A `max_doc` bitset is allocated per segment.

**In the parser.** It needs `allow_regexes()`.

### Applies to codectx?

**Yes, but only as the folder filter on an untokenized path field**, and a cheaper option exists.
- The harness uses `RegexQuery::from_pattern(escape(P) + "/.*", path_s)` OR `TermQuery(path_s, P)` under `ConstScoreQuery(…, 0.0)`. That is exactly the product's whole-folder rule: `doc` never matches `docs/`.
- Because the regex is anchored, `P/.*` is a pure prefix walk.
- On text columns, a regex would bypass the tokenizer and scoring. It is not a product feature.

**Metric.** Filtered-run latency. Hits must equal the product's.

**Variant (existing).** `tquery <dir> queries-doc.txt 5 library --path-prefix pkg/api`. Also run `--path-prefix doc` (expect 0 when only `docs/` exists) and an exact file path (expect its documents). Compare with the RangeQuery variant below.

---

## FuzzyTermQuery

- **What it is.** It matches terms within a Levenshtein distance of the query term, optionally as a prefix, through a Levenshtein DFA.
- **API** (`src/query/fuzzy_query.rs`):
  - `FuzzyTermQuery::new(term: Term, distance: u8, transposition_cost_one: bool)` (:93)
  - `FuzzyTermQuery::new_prefix(term, distance, transposition_cost_one)` (:103)

### Behaviour

- **Distance.** Only 0, 1 or 2. The builders are a static `[[OnceCell; 2]; 3]` (:113-117), and a larger distance gives `InvalidArgument("Levenshtein distance of {} is not allowed…")` (:118-126). The term must be a string (:153).
- **Score.** Constant. `weight` ignores `EnableScoring` (:178) and returns an `AutomatonWeight`, so every fuzzy match scores 1.0 × boost. The crate's own test asserts `score == 1.0` (:301-302).
- **In the parser.** `set_field_fuzzy` turns every single-term literal on that field into this query. Fuzzy terms therefore lose BM25.

### Applies to codectx?

**No under the current rules.** The product's lexical match is exact tokens, so fuzzy matching would change the match set, and constant scoring would erase the ranking. It is a candidate only for an opt-in "did you mean" path, outside the complete-answer contract.

**Metric.** Zero-hit queries (fewer) against MRR@10 and S@1 (worse ranking) and mean hits (more).

**Variant.** Needs a new mode, `library-fuzzy`:
- Each single-token term per column becomes `BooleanQuery[(Should, TermQuery), (Should, BoostQuery(FuzzyTermQuery::new(t, 1, true), 0.1))]`.
- Run it on `queries-subword.txt` and `queries-name.txt`, then `teval` the zero-hit and MRR@10 deltas against `library`.

---

## RangeQuery (and InvertedIndexRangeQuery)

- **What it is.** It matches documents whose term or value falls inside the bounds. It runs on the fast field when it can, and on the term dictionary otherwise.
- **API** (`src/query/range_query/range_query.rs`):
  - `RangeQuery::new(lower_bound: Bound<Term>, upper_bound: Bound<Term>)` (:80), `field()` and `value_type()`
  - `InvertedIndexRangeQuery::new(lower, upper)` (:134) and `limit(&mut self, u64)` (:144)

### Behaviour

**Execution path.**
- If the field is fast and of a supported type (Str, U64, I64, F64, Bool, Date, Json, IpAddr, Bytes; `range_query/mod.rs:13-26`), it uses `FastFieldRangeWeight` (:108-109).
- For a `Str` fast field, the bounds become dictionary ordinals (`term_bounds_to_ord`), and the column is scanned for that ordinal range (`range_query_fastfield.rs:159-174`). This is lazy, but linear in the number of documents in the segment.
- Otherwise it uses `InvertedIndexRangeWeight`, which ORs the postings of every term in range into a `max_doc` bitset (:216-243).

**Score.** Constant (`ConstScorer`).

**Parser.** Ranges need an explicit field.

### Applies to codectx?

**Yes, as an alternative folder filter.** `path_s` is `STRING | FAST`, so `RangeQuery[Included(path_s:"P/"), Excluded(path_s:"P0")]` (`'0'` is the byte after `/`), OR `TermQuery(path_s:P)`, gives the same whole-folder set as the regex without building an automaton.
- On a fast field it scans the column. Use `InvertedIndexRangeQuery` to force the dictionary path instead, and benchmark both.
- `token_count` ranges have no product use.

**Metric.** Filtered latency, with identical hits.

**Variant.** Needs a new mode flag, `--path-filter regex|ffrange|invrange`. Today `library` hard-codes `regex`.
- `ffrange` = `RangeQuery` on `path_s`. `invrange` = `InvertedIndexRangeQuery` on `path_s`. Both are OR'd with `TermQuery(path_s, P)` under `ConstScoreQuery(…, 0.0)`.
- Run `tquery <dir> queries-doc.txt 5 library --path-prefix pkg/api`, plus a deep and a shallow prefix (the shallow one matches most of the corpus). Compare medians; hits must be identical.

---

## ExistsQuery

- **What it is.** It matches documents that have any value in a fast field.
- **API** (`src/query/exist_query.rs`): `ExistsQuery::new_exists_query(field: String)` (:48) and `ExistsQuery::new(field: String, json_subpaths: bool)` (:64).

### Behaviour

- The field must be fast, otherwise `SchemaError("Field {} is not a fast field.")` (:79-84).
- A full column gives `AllScorer`. Fewer than 4 sparse columns use `ExistsDocSet` (:101-141).
- The score is constant.
- The parser cannot produce it: `field:*` is rejected (`query_parser.rs:859-864`).

### Applies to codectx?

**No product use.** Every document has `kind`, `path` and `id`. It would matter only for an optional column.

**Metric.** None.

**Variant.** None needed.

---

## DisjunctionMaxQuery (tie_breaker)

- **What it is.** It scores the best-matching clause, plus `tie_breaker` × the other matching clauses.
- **API** (`src/query/disjunction_max_query.rs`): `DisjunctionMaxQuery::new(Vec<Box<dyn Query>>)` (:127), which sets tie_breaker = 0, and `DisjunctionMaxQuery::with_tie_breaker(Vec<Box<dyn Query>>, tie_breaker: Score)` (:116).

### Behaviour

**Score.** It is a `BooleanWeight` of `Should` clauses with `DisjunctionMaxCombiner` (:93-105). The score is `max + (sum − max) × tie_breaker` (`score_combiner.rs:80-94`).

**Hazard (read from source, not yet run).**
1. When every disjunct is a `TermQuery` reading frequencies, `BooleanWeight` builds a `TermUnion` (`boolean_weight.rs:57-66`).
2. `for_each_pruning` then calls `block_wand` (:524-525), which **sums** the term scores (`block_wand.rs:200-204`) and ignores the dis-max combiner.
3. `TopDocs` ordered by score uses `for_each_pruning` (`sort_by_score.rs:41,50`). So in that path, a dis-max over per-field `TermQuery(WithFreqs)` would score by sum.
4. The exhaustive `for_each` path uses `BufferedUnionScorer` with the combiner, which is correct.

The crate's doc example builds its disjuncts with `IndexRecordOption::Basic`, which avoids the `TermUnion` path (:44-51).

A 3-document test settles it: compare `TopDocs` scores with `explain()`. It also sets which form the `library` arm may use.

### Applies to codectx?

**As a comparison arm only.** Dis-max over the five columns per term (tie_breaker around 0.1-0.3) is the classic multi-field alternative to summed per-field BM25. It is still not BM25F, which saturates the weighted tf once.
- It must not be used with `TopDocs` over pure `TermQuery(WithFreqs)` disjuncts until the hazard above is ruled out.
- Wrapping each disjunct in `BoostQuery` does not avoid the hazard. `BoostWeight` passes the boost to `TermWeight::scorer`, which still returns a `TermScorer` (`boost_query.rs:70-72`).

**Metric.** MRR@10, S@1 and S@10.

**Variant.** Needs a new mode, `library-dismax <tie>`:
- Per term: `DisjunctionMaxQuery::with_tie_breaker([BoostQuery(TermQuery|PhraseQuery(field), w) for 5 fields], tie)`, with tie ∈ {0.0, 0.1, 0.3}.
- Run it with the `library` collector and with the `library-all` collector. A score mismatch between the two on single-token queries confirms the hazard.
- Then `teval` against `library` and `product` on `queries-name.txt` and `queries-doc.txt`.

---

## BoostQuery

- **What it is.** It multiplies a sub-query's score by a constant.
- **API.** `BoostQuery::new(query: Box<dyn Query>, boost: Score)` (`src/query/boost_query.rs:20`).

### Behaviour

- With scoring on, it wraps the weight in `BoostWeight`. With scoring off, it returns the inner weight unchanged (:41-48).
- `BoostWeight::scorer` passes `boost × self.boost` down (:70-72). A `TermWeight` folds the boost into its `Bm25Weight` (`term_weight.rs:203`), so a boosted term is still a `TermScorer` and stays eligible for block-WAND inside a union.
- `BoostWeight` does not override `for_each_pruning`. A top-level `BoostQuery` therefore uses the generic, unpruned loop (`weight.rs:123-131`).
- The boost is an f32 multiply.

### Applies to codectx?

**Yes. It is how the column weights 5/5/2/2/1 enter in the library arms.** It weights each field's BM25 score after saturation. That is unlike BM25F, which weights tf before saturation, and it is the main reason `library` ranks differently from `product`.

**Metric.** MRR@10 and S@1 (the weight placement).

**Variant (existing).** `library` vs `product` on all three query sets, through `teval`. Optionally, needs a new mode flag `--weights a/b/c/d/e` to sweep the per-field boosts for the library arms (for example 1/1/1/1/1 and 5/5/2/2/1), reporting MRR@10.

---

## ConstScoreQuery

- **What it is.** It keeps a sub-query's match set and assigns a fixed score.
- **API.** `ConstScoreQuery::new(query: Box<dyn Query>, score: Score)` (`src/query/const_score_query.rs:19`).

### Behaviour

- With scoring on, the inner weight is still built **with scoring enabled** (:40-47). For a `TermQuery`, that still computes its BM25 statistics.
- `ConstScorer` reports `boost × score` (:66-68).
- The doc comment says it "can avoid unnecessary score computation on the wrapped query" (:7-8). The inner scorer still iterates. Use `IndexRecordOption::Basic` on inner `TermQuery`s so frequencies are not decoded.

### Applies to codectx?

**Yes. It is the filter wrapper.** `kind` and folder filters as `(Must, ConstScoreQuery(filter, 0.0))` add no score and restrict the match set. Statistics stay unfiltered, matching the product's rule that df and N do not depend on the filter.

**Metric.** Filtered latency. The ranking must equal the unfiltered ranking restricted to the filter set.

**Variant (existing).** `tquery <dir> queries-doc.txt 5 library --kind function --kind method --path-prefix pkg/api`. Check with `tcompare` that its order equals the unfiltered `library` order after removing non-matching documents.

---

## MoreLikeThisQuery

- **What it is.** It builds a SHOULD query of a document's most distinctive terms.
- **API** (`src/query/more_like_this/query.rs`):
  - `MoreLikeThisQuery::builder()` (:41)
  - Builder setters: `with_min_doc_frequency(u64)` (:86), `with_max_doc_frequency(u64)`, `with_min_term_frequency(usize)`, `with_max_query_terms(usize)`, `with_min_word_length(usize)`, `with_max_word_length(usize)`, `with_boost_factor(f32)` and `with_stop_words(Vec<String>)` (:151)
  - Finishers: `with_document(DocAddress)` (:164) or `with_document_fields(Vec<(Field, Vec<V>)>)` (:178)

### Behaviour

**Defaults** (`more_like_this.rs:66-78`): `min_doc_frequency 5`, `min_term_frequency 2`, `max_query_terms 25`, `boost_factor 1.0`.

**Term selection.**
- Terms are scored `tf × idf`. For N it uses `num_docs`, which counts live documents only (:305-309), while df comes from `Searcher::doc_freq`, which includes deleted ones.
- Terms are visited from a `HashMap` in arbitrary order (:311). The heap keeps `len() > limit` (:350), so up to `limit + 1` terms. When scores tie, which terms survive depends on that arbitrary order.

**Query shape.** `TermQuery(Basic)` × `BoostQuery(score × factor / best)` (:109-122).

**Requirements.**
- `with_document` needs stored fields, otherwise it errors ("The document may not have stored fields", :145-150).
- It needs scoring enabled (`query.rs:50-53`).

### Applies to codectx?

**No.** It has default caps (`max_query_terms`, df thresholds) and ties that depend on hash order, which violates completeness and determinism.
- The product does not store `body` (the harness indexes `body` without storing it), so `with_document` would see only stored columns.
- A "related symbols" feature would need its own term selection.

**Metric.** None for lexical search.

**Variant.** None.

---

## AllQuery

- **What it is.** It matches every document, with score 1.0.
- **API.** The unit struct `AllQuery` (`src/query/all_query.rs:12`).

### Behaviour

- `AllScorer::new(max_doc)` covers every document ID, deleted ones included. The collector drops deleted documents.
- A boost other than 1.0 wraps it in `BoostScorer` (:24-31).
- The parser's `*` produces it, and lenient mode adds it to repair all-negative queries.

### Applies to codectx?

**Yes, as the harness's product-mode driver.** Product mode searches `AllQuery` with `requires_scoring() = false` and a collector that overrides `collect_segment`. That collector ignores the weight and reads postings, the alive bitset and fast fields itself. It is the mechanism that lets exact BM25F, live-only df and f64 scoring run on a tantivy index.

**Metric.** Product-mode latency (cold and warm).

**Variant (existing).** `tquery <dir> queries-doc.txt 5 product` and the filtered form `… product --kind function --path-prefix pkg/api`.

---

## EmptyQuery

- **What it is.** It matches nothing.
- **API.** The unit struct `EmptyQuery` (`src/query/empty_query.rs:12`). Its weight is `EmptyWeight`, its scorer `EmptyScorer`, and `count` returns 0 (:15-22).

### Behaviour

- `QueryParser` returns it when the whole query trims to nothing, for example text that tokenizes to no tokens (`query_parser.rs:1078`).
- `BooleanWeight` collapses a MUST `EmptyScorer` to an empty result (`boolean_weight.rs:236-238`).

### Applies to codectx?

**Yes, for the empty case.** A query whose terms all tokenize away (punctuation only) should answer zero hits. `EmptyQuery` is the explicit form, rather than an empty `BooleanQuery` (which gives `EmptyScorer` anyway, `boolean_weight.rs:418-419`).

**Metric.** Zero-hit queries must be correct (no error on all-punctuation input).

**Variant (existing).** Add all-punctuation lines (`::`, `()`, `""`) to a query file and run `tquery <dir> <file> 1 library|library-all|product`. Each line must record 0 hits without error.

---

## Summary for adoption

| Query | Product role | Product rule at risk |
|---|---|---|
| TermQuery | per-column single-token match; `kind` / exact-path filter | none in matching. Its BM25 is per-field, includes deleted docs, f32 |
| BooleanQuery | the query tree; filters as `Must` | the default `minimum_should_match` = 0 with any `Must`, so nest the text query |
| BoostQuery | column weights in the library arms | the weight applies after saturation, so it is not BM25F |
| ConstScoreQuery | filter wrapper | none |
| PhraseQuery | quoted phrases | idf is the sum of term idfs; positions uncapped (product caps at 512); u8 slop accumulator |
| RegexQuery / RangeQuery | folder filter on `path_s` | none (anchored or ordinal range). Benchmark both |
| TermSetQuery | multi-kind filter | none (unscored) |
| AllQuery | product-mode driver | none |
| EmptyQuery | empty query | none |
| QueryParser | not for user text | `:`, `-`, `AND` and punctuated tokens change meaning; summed per-field BM25 |
| DisjunctionMaxQuery | comparison arm only | hazard: block-WAND may sum instead of dis-max under `TopDocs` |
| PhrasePrefixQuery | not adoptable | 50-term per-segment expansion cap |
| FuzzyTermQuery | not adoptable now | constant score; changes the match set |
| MoreLikeThisQuery | not adoptable | default caps; ties depend on hash order |
| ExistsQuery | no use | n/a |
