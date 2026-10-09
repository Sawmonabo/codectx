# 03 — Tokenizers and token filters, registration, index-time vs query-time analysis, and code-aware splitting (tantivy 0.26.2)

Sources:
- The 0.26.2 crate source (`tantivy-0.26.2/src/...`) and its tokenizer API crate (`tantivy-tokenizer-api-0.7.0/src/lib.rs`).
- `examples/custom_tokenizer.rs`, `examples/pre_tokenized_text.rs` and `examples/stop_words.rs`, all shipped in the crate.
- `ARCHITECTURE.md` and `CHANGELOG.md`.
- The bench arm's probe note `../harness/01-tokenizer-probe.md`.
- The docs root is https://docs.rs/tantivy/0.26.2/tantivy/tokenizer/index.html.

**codectx context.**
- codectx tokenizes all five search columns (name, qualified_name, signature, path, body), and every query, with SQLite FTS5 `unicode61` at its defaults: categories `L* N* Co`, `remove_diacritics=1`.
- It truncates tokens at 32 768 bytes.
- Its scoring is BM25F over a per-term, weighted sum of tf across the five columns, with the document length taken from `token_count` (`internal/search/bm25.go`; `../harness/06-extension-points.md`).
- The bench arm reproduces unicode61 exactly with a custom tokenizer, with 0 anomalies over every codepoint and 2 known differing lines out of 303 391 tokens (`../harness/01-tokenizer-probe.md`).

**Harness vocabulary.** `tindex <docs> <dir> <threads> [default|large|MB] [unit|every:K|end] [--tokenizer unicode61|code]`. A knob without a flag is written as an exact setting followed by "needs a harness flag".

---

## 1. The pipeline and its contracts

**Exact API.**
- `Token { offset_from, offset_to, position, text: String, position_length }` (`tantivy-tokenizer-api-0.7.0/src/lib.rs:15-28`). `Token::default()` has `position: usize::MAX` and `position_length: 1` (`lib.rs:30-40`), so the first `position.wrapping_add(1)` gives 0.
- `trait Tokenizer: 'static + Clone + Send + Sync { type TokenStream<'a>: TokenStream; fn token_stream<'a>(&'a mut self, text: &'a str) -> Self::TokenStream<'a>; }` (`lib.rs:56-60`).
- `trait TokenStream { fn advance(&mut self) -> bool; fn token(&self) -> &Token; fn token_mut(&mut self) -> &mut Token; fn process(..) }` (`lib.rs:119-150`).
- `trait TokenFilter: 'static + Send + Sync { type Tokenizer<T: Tokenizer>: Tokenizer; fn transform<T: Tokenizer>(self, tokenizer: T) -> Self::Tokenizer<T>; }` (`lib.rs:152-157`).
- `TextAnalyzer::builder(tokenizer).filter(f)….build()`, and `.dynamic()` / `.filter_dynamic(f)` for chains chosen at run time (`tantivy-0.26.2/src/tokenizer/tokenizer.rs:63,92,100,111,116`). Any `Tokenizer + Clone` converts with `TextAnalyzer::from` (`tokenizer.rs:55`).
- The tokenizer API moved to its own crate in 0.20 "with a stable API" (CHANGELOG, 0.20). Dynamic filters were added in 0.21 (CHANGELOG, 0.21).

**How the indexer consumes tokens** (`src/postings/postings_writer.rs:128-161`):
- **Position.** Each token goes to `start_position = end_position_so_far + token.position`. The position is relative to the value, and a filter that gives two tokens the same `position` stacks them.
- **Several values.** Values of one field in one document are separated by `POSITION_GAP = 1` (`postings_writer.rs:18,158`).
- **Overlong tokens are dropped.** A token over `MAX_TOKEN_LEN = u16::MAX - 5` = 65 530 bytes is **dropped with a warning, not truncated** (`postings_writer.rs:140-149`, `src/tokenizer/mod.rs:163-168`).
- **The fieldnorm counts emitted tokens.** The field's fieldnorm is the count of emitted tokens, stacked ones included (`num_tokens += 1` per token, `postings_writer.rs:155`; recorded at `src/indexer/segment_writer.rs:218-221`).
- **Filtered tokens leave gaps.** A filter that *removes* a token still leaves the tokenizer's position counter advanced, so the dropped token leaves a gap: a phrase across it no longer matches as adjacent. The fieldnorm counts only the survivors.

**Applies to codectx?** Yes. This is where FTS5 parity is won or lost.
- **Parity holds.** FTS5's 32 768-byte truncation is done inside the bench tokenizer, so tantivy's 65 530-byte drop never fires (`../harness/01-tokenizer-probe.md`).
- **Stacking moves stock BM25.** Any filter that stacks extra tokens changes stock BM25 (longer fieldnorm) but not product mode, which uses `token_count`.

---

## 2. Registering a custom tokenizer; index-time vs query-time analysis

**Exact API.**
- `index.tokenizers().register(name, analyzer)` (`src/tokenizer/tokenizer_manager.rs:34-41`, `src/index/index.rs:403`).
- A field names its analyzer with `TextFieldIndexing::set_tokenizer(name)` (`src/schema/text_options.rs:224`).
- `Index::set_tokenizers(TokenizerManager)` replaces the whole manager (`index.rs:398`).
- Fast-text normalizers live in a second manager, `Index::fast_field_tokenizer()` / `set_fast_field_tokenizers` (`index.rs:408-415`).
- `IndexBuilder::tokenizers(..)` / `fast_field_tokenizers(..)` set both managers at creation (`index.rs:141,147`).

**Behaviour.**
- **Registration is not persisted.** The schema stores only the tokenizer *name*. `Index::open` starts from `TokenizerManager::default()` (`index.rs:391`), so a custom analyzer must be registered again after every open, before any writer or query parser is built. The bench arm does this in `open_index` (`../harness/source/src/lib.rs`).
- **A missing name is an error at index time.** `SegmentWriter::for_segment` looks up every text field's analyzer and fails with `SchemaError("Error getting tokenizer for field: …")` (`src/indexer/segment_writer.rs:80-101`). This has been so since 0.21: "Return an error when tokenizer is not found while indexing" (CHANGELOG, 0.21).
- **Each segment writer clones the manager's analyzers** (`segment_writer.rs:75`). Per-thread state such as buffers lives in the cloned `Tokenizer`.
- **Queries use the same name by default.** `QueryParser::for_index(&index, fields)` takes `index.tokenizers().clone()` (`src/query/query_parser/query_parser.rs:279-281`). `Index::tokenizer_for_field(field)` returns the field's index-time analyzer (`index.rs:418-445`). **There is no separate "search analyzer" setting.**
- **Two ways to analyze queries differently:**
  1. Build the parser with `QueryParser::new(schema, default_fields, other_manager)` (`query_parser.rs:254-262`), where `other_manager` maps the same name to another analyzer.
  2. Bypass the parser: tokenize yourself and build `TermQuery` / `PhraseQuery` / `BooleanQuery` from `Term::from_field_text`. Product mode does this, because it implements codectx's own query rules.
- **Multi-token literals become phrases.** A query literal that tokenizes to more than one token becomes a **phrase**, not an OR, and needs positions on the field (`query_parser.rs:958-995`). Unquoted `foo.bar` is therefore the phrase "foo bar" under the parser. A stacked token keeps its position in a parsed phrase (`query_parser.rs:971`).

**Applies to codectx?** Yes.
- **The query side must call exactly the same `Unicode61` code as the index side.** Product mode does, because it tokenizes queries itself.
- **The parser's phrase rule is a known difference.** It differs from codectx's query encoding, so a library mode that uses `QueryParser` answers a different query than codectx does (`../harness/00-library-arm.md`, departures). The parser's rule buys phrase precision for punctuated input such as `foo.bar`; codectx's OR rule buys recall.

**Metric.** Quality (hit sets), and query latency (parser vs hand-built queries).

**Benchmark variant.** Already covered: `tquery … library|library-all|product` builds the query once and shares it across the library modes (`../harness/00-library-arm.md`). To isolate the parser difference, add a `--parser tantivy` library-mode variant that feeds raw query text to `QueryParser::for_index(&index, text_fields)`, and run `tcompare` against `product`. Needs a harness flag.

---

## 3. Shipped tokenizers

Pre-registered names (`src/tokenizer/tokenizer_manager.rs:53-80`):
- `raw`.
- `default` = `SimpleTokenizer` + `RemoveLongFilter::limit(40)` + `LowerCaser`.
- `en_stem` = `default` + `Stemmer::new(Language::English)`. It exists only with the `stemmer` feature, which became a feature flag in 0.26 (CHANGELOG, 0.26).
- `whitespace`.

| tokenizer | exact API | behaviour (cited) | codectx |
|---|---|---|---|
| `SimpleTokenizer` | `SimpleTokenizer::default()` | A token is a maximal run of `char::is_alphanumeric()`. Everything else separates, including `_` (`simple_tokenizer.rs:31-56`). Positions are 0, 1, 2… (`:44`). | Close to unicode61, but not equal: `is_alphanumeric` is Unicode Alphabetic + Numeric, while unicode61 is `L* N* Co` plus diacritic folding. It also does not fold case by itself. It buys speed and simplicity, but gives different hit sets from FTS5 unicode61. |
| `WhitespaceTokenizer` | `WhitespaceTokenizer::default()` | Splits on `is_ascii_whitespace()` only (`whitespace_tokenizer.rs:33,45`). | No. It keeps `foo.bar(` as one token. |
| `RawTokenizer` | `RawTokenizer::default()` | The whole value is one token (`raw_tokenizer.rs:3`). This is what `STRING` uses (`text_options.rs:264-268`). | Yes for `id`, `kind`, `unit` and `path_s` (exact keys). |
| `RegexTokenizer` | `RegexTokenizer::new(pattern) -> Result<_>`; a bad pattern gives `InvalidArgument` (`regex_tokenizer.rs:57-64`) | Each regex match is a token. Text between matches is skipped. **An empty match ends the stream** (`regex_tokenizer.rs:88-106`). The `Regex` is cloned per `token_stream` (`:72`). Added in 0.20 (CHANGELOG, 0.20). | Possible for code-part splitting (§6). The Rust `regex` crate has no look-around, so camelCase rules must be written without it. |
| `NgramTokenizer` | `NgramTokenizer::new(min, max, prefix_only) -> Result<_>`, `::all_ngrams(min, max)`, `::prefix_only(min, max)`. It errors if `min == 0` or `min > max` (`ngram_tokenizer.rs:96-130`). | It n-grams **the whole value**, by codepoint, with every token at position 0 (`ngram_tokenizer.rs:4-8,162-173`). It is not per word, so put it behind a word tokenizer, or use it on short fields such as `name`. | Maybe: substring and partial-identifier match on `name` / `qualified_name` as an extra field. Large disk cost; positions are meaningless. |
| `FacetTokenizer` | used automatically by facet fields | Emits the path and every ancestor (`facet_tokenizer.rs:4-11`). | Yes, for the folder filter; see `02-settings-fields.md` §5. |
| `EmptyTokenizer` | `empty_tokenizer.rs` | Emits nothing. | No. |
| `PreTokenizedString` | `PreTokenizedString { text: String, tokens: Vec<Token> }` (`tokenized_string.rs:11-16`), added as a field value; example `examples/pre_tokenized_text.rs` ("text which is already split into tokens by some external tool", `:3-5`) | The indexer skips the analyzer and indexes the given tokens with their positions (`segment_writer.rs:199-207`). | **Yes, as a design option**: codectx could pass the exact tokens its own tokenizer produced, so parity holds by construction (§6). |

---

## 4. Shipped token filters

| filter | exact API | behaviour (cited) | codectx |
|---|---|---|---|
| `LowerCaser` | `LowerCaser` | ASCII fast path with `make_ascii_lowercase`. Otherwise it maps each char through `char::to_lowercase`, "Contrary to the std, we do not take care of sigma special case" (`lower_caser.rs:44-66`). | No. unicode61 case-folds per codepoint from its own probed tables (`../harness/01-tokenizer-probe.md`). Rust's `to_lowercase` is a different mapping, and equality was never checked. The bench arm folds inside its tokenizer. |
| `AsciiFoldingFilter` | `AsciiFoldingFilter` | Converts non-Basic-Latin letters, digits and symbols "into their ASCII equivalents, if one exists" (`ascii_folding_filter.rs:5-9`). | No. unicode61's `remove_diacritics=1` is a different, table-defined fold, so this would change hit sets. |
| `RemoveLongFilter` | `RemoveLongFilter::limit(n)` | Keeps a token only if `text.len() < n` **bytes** (`remove_long.rs:17-36`). Dropped tokens leave position gaps (§1). | `default` uses `limit(40)`, which drops every identifier, hash or path segment of 40 bytes or more, with no warning. That buys smaller dictionaries on prose with junk tokens. For codectx, which keeps every token up to 32 768 bytes, it costs recall on long identifiers, so it should not be in a codectx analyzer. |
| `Stemmer` | `Stemmer::new(Language)`; 18 languages (Arabic … Turkish, `stemmer.rs:12-31`); feature `stemmer` | Snowball stemming of each token. | No for parity, since FTS5 unicode61 does not stem. At most a separate quality experiment, and code identifiers are not natural language. |
| `StopWordFilter` | `StopWordFilter::new(Language) -> Option<Self>` (feature `stopwords`), `StopWordFilter::remove(words)` (`stop_word_filter/mod.rs:33-37,67`) | Drops listed words. Removed words leave position gaps. | No. codectx keeps every token, and complete answers include "the". |
| `SplitCompoundWords` | `SplitCompoundWords::from_dictionary(words) -> Result<_>`, `::from_automaton(AhoCorasick)` (`split_compound_words.rs:56,77`) | Splits a token **only if it "can be fully decomposed into consecutive matches into the given dictionary"** (`:5-9`). It uses leftmost-longest Aho–Corasick (`:62`). The parts **replace** the original token, and every part keeps the original's position (`:125-158`). | No for camelCase. It needs a dictionary of every identifier part and drops the whole token. |
| `AlphaNumOnlyFilter` | `AlphaNumOnlyFilter` | Drops tokens that are not entirely `is_ascii_alphanumeric()` (`alphanum_only.rs:26-37`). | No. It would drop every non-ASCII identifier. |

There is **no built-in camelCase / snake_case / code-identifier splitter** in 0.26.2. The full list of exports is `src/tokenizer/mod.rs:143-161`.

---

## 5. What the bench arm already does (baseline for variants)

- **`unicode61`.** A custom `Tokenizer` that reproduces FTS5 unicode61 from probed tables: a token is a maximal run of non-separators, each codepoint is folded on its own, and an all-folded-away run takes no position. It also cuts at 32 768 bytes on a char boundary (`../harness/01-tokenizer-probe.md`; `../harness/source/src/unicode61.rs:1-5,77`).
- **`code`.** It emits every unicode61 token and, when the original spelling has two or more camelCase parts, each folded part **stacked at the same position** as the whole token (`../harness/source/src/unicode61.rs:187-191`). snake_case needs nothing extra, because `_` is already a separator.
- **Known costs** of `code`, from `../harness/00-library-arm.md` departure 8: stacking inflates fieldnorms, which moves stock BM25 but not product mode, and it lets phrases span identifiers.
- **Measured:** subword R@200 was 0.56 for unicode61 vs 0.97 for code on a synthetic vocabulary. The report says plainly that this "says nothing about quality".

---

## 6. Code-aware splitting options (designs, with the exact mechanism)

**A. Stacked parts in the same field** (the current `code`).
- Mechanism: `Token { position: p_whole, .. }` for each part (§1).
- Pros: one field, so df and tf stay inside the five-column BM25F.
- Cons: fieldnorm and stock-BM25 drift; phrase queries can match across parts.

**B. A separate parts field per column** (`name_parts`, … or one `parts` field).
- Mechanism: the same text indexed twice, under two tokenizer names. ARCHITECTURE.md says tantivy imposes a 1:1 field mapping and "it is up to the user to duplicate field / concatenate fields before feeding them to tantivy" (`ARCHITECTURE.md:116-120`).
- Pros: whole-token statistics stay exact and identical to FTS5. Part matches can be weighted separately.
- Cons: more disk and index time. Product scoring needs a rule for how part tf joins the BM25F sum (a product decision, not a library one).

**C. A `RegexTokenizer`-based part splitter** behind the unicode61 fold, for example the pattern `[A-Z]+[a-z0-9]*|[a-z0-9]+` on the original spelling.
- Pros: no custom code.
- Cons: without look-around it cannot split `HTTPServer` into `HTTP` + `Server`. It must still be followed by unicode61's per-codepoint fold, so it is not simpler than the existing custom filter.

**D. Pre-tokenized input** (`PreTokenizedString`).
- Mechanism: codectx's own tokenizer, or the SQLite one, produces `(position, term)` lists, and tantivy indexes them verbatim.
- Pros: parity by construction, for both whole tokens and any part scheme, with no tokenizer drift between engines.
- Cons: the queries must be tokenized the same way outside tantivy (product mode already does this). Stored or snippet offsets come from the caller.

**Applies to codectx?** Yes. A, B and D are real candidates; C is not worth it.

**Metric.** Quality (recall and MRR on subword queries), disk (`.idx`, `.pos`, `.term`), index time, and query latency (more terms per query in B).

**Benchmark variants.**
- **A vs baseline** (existing flags): `tindex docs.jsonl ix-u61 8 large end --tokenizer unicode61` vs `tindex docs.jsonl ix-code 8 large end --tokenizer code`. Then `tqrels docs.jsonl q 7`, run `tquery` in all modes, and `teval` on name, doc and subword. Use the real export, not the synthetic fixture, so the numbers mean something about quality.
- **B:** `--tokenizer u61+parts`, which adds one `parts` field (`WithFreqsAndPositions`, fieldnorms off) carrying only the camelCase parts. Needs a harness flag. Compare disk and `teval` against A.
- **D:** `--pretokenized`, where `tindex` feeds `PreTokenizedString` built from the same `unicode61` function instead of registering the analyzer. Needs a harness flag. The index must be byte-identical in hits to `--tokenizer unicode61` (`tcompare` 200/200); report the `-add` time difference, which is the cost of the analyzer call against building token vectors.
- **Negative control:** `--tokenizer default` (tantivy's own `default`). Needs a harness flag. Report how many documents lose a token to `RemoveLongFilter::limit(40)` (recount tokens against `token_count`; the `tindex` recount already exists) and the `teval` delta. This measures what the built-in `default` gains (index size, speed) and loses (recall) against unicode61.
- **Optional:** an `name_ngram` field via `TextAnalyzer::builder(NgramTokenizer::all_ngrams(3, 3)?).filter(LowerCaser).build()` on `name` only, for partial-identifier recall, with its disk cost reported. Needs a harness flag.

---

## 7. Web sources consulted (with credibility)

**Primary sources: official.**
- docs.rs, `tantivy::tokenizer` at 0.26.2: https://docs.rs/tantivy/0.26.2/tantivy/tokenizer/index.html.
  - It lists 21 structs, which match the exports at `src/tokenizer/mod.rs:143-161`, plus `Language`, `MAX_TOKEN_LEN`, and the traits `TokenFilter`, `TokenStream` and `Tokenizer`.
  - "You can write your own tokenizer by implementing the Tokenizer trait".
  - The `default` analyzer "will chop your text on punctuation and whitespaces".
- tantivy `examples/custom_tokenizer.rs`, `examples/pre_tokenized_text.rs` and `examples/stop_words.rs`, shipped in the 0.26.2 crate (primary, local).
- CHANGELOG 0.20 (regex tokenizer #1759, tokenizer-api crate #1767), 0.21 (dynamic filters #2110, error on a missing tokenizer #2093) and 0.26 (stemmer behind a feature #2791). Primary.

**Secondary sources: other code-search tools built on tantivy (design evidence, not benchmarks).**
- ygrep README: https://github.com/yetidevworks/ygrep.
  - A custom tokenizer "preserves code characters (`$`, `@`, `#`, `-`, `_`) as part of tokens".
  - "camelCase and snake_case identifiers are split into subtokens", so `send` finds `sendCampaign`.
  - Its index-size table (for example 36.9 → 21.5 MB on one project between its own releases) is the tool's own measurement, on an M4 Max.
  - It does not state how subtokens are positioned. It is evidence that design A or B (§6) is the common choice, not evidence for which one is better.
- open-kioku PR #573, September 27 2026: https://github.com/shivyadavus/open-kioku/pull/573.
  - Its code tokenizer "splits on `::`, `.`, `-` and `/`, and adds CamelCase/snake_case parts".
  - Moving a whole record from indexed to stored-only, and indexing a name/qualified-name/signature/path field instead, moved holdout R@5 0.722 → 0.778 and lexical dev R@5 0.860 → 0.833.
  - Another project's measured harness. It shows that the tokenizer and the field choice move quality in both directions, which is why §6's variants must be judged with `teval` on the real export.

**Searched for, not found.** A published comparison of stacked parts (design A) against a separate parts field (design B) for code search on tantivy, or a camelCase filter shipped by tantivy itself.
