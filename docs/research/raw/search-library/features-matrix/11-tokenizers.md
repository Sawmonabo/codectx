# 11 — Tokenizers and token filters

Version: tantivy 0.26.2 and its tokenizer API crate `tantivy-tokenizer-api-0.7.0`. Paths are inside `tantivy-0.26.2/` unless named. Docs: https://docs.rs/tantivy/0.26.2/tantivy/tokenizer/index.html

## The pipeline
- `TextAnalyzer::builder(tokenizer).filter(f)...build()`; `.dynamic()` / `.filter_dynamic(f)` build a boxed chain chosen at run time (`src/tokenizer/tokenizer.rs:63-116`). An analyzer is registered by name on `index.tokenizers()` (a `TokenizerManager`) and named by `TextFieldIndexing::set_tokenizer`.
- A `Token` carries `offset_from`, `offset_to`, `position`, `text`, `position_length` (`tantivy-tokenizer-api-0.7.0/src/lib.rs:15-28`). Positions are relative within one value; the indexer adds the running end position, so a filter that emits two tokens with the same `position` stacks them at one position (`src/postings/postings_writer.rs:152-154`). Multiple values of one field are separated by `POSITION_GAP = 1` (`:18, :158`).
- The fieldnorm records the number of emitted tokens, stacked ones included (`num_tokens += 1` per token, `src/postings/postings_writer.rs:155`; recorded at `src/indexer/segment_writer.rs:218-220`). A filter that adds sub-tokens therefore lengthens the document for BM25.
- Tokens longer than `MAX_TOKEN_LEN = u16::MAX - 5` bytes (65 530) are DROPPED with a warning, not truncated (`src/tokenizer/mod.rs:165-168`, `src/postings/postings_writer.rs:141-147`). SQLite FTS5, which codectx uses, truncates instead; the bench's Rust arm's tokenizer truncates at 32 768 itself, so this limit is never reached there.
- `PreTokenizedString { text, tokens }` indexes tokens produced outside tantivy (`src/tokenizer/tokenized_string.rs:9-16`, example `examples/pre_tokenized_text.rs`).

## Built-in tokenizers
| Tokenizer | Behaviour | Source |
|---|---|---|
| `SimpleTokenizer` | splits on every char that is not `char::is_alphanumeric()` | `src/tokenizer/simple_tokenizer.rs:5, 34, 46` |
| `WhitespaceTokenizer` | splits on whitespace only | `src/tokenizer/whitespace_tokenizer.rs:5` |
| `RawTokenizer` | the whole value is one token (used by `STRING`) | `src/tokenizer/raw_tokenizer.rs` |
| `RegexTokenizer::new(pattern)` | each regex match is a token | `src/tokenizer/regex_tokenizer.rs:57` |
| `NgramTokenizer::new(min, max, prefix_only)`, `all_ngrams`, `prefix_only` | character n-grams; errors on bad arguments | `src/tokenizer/ngram_tokenizer.rs:96-130` |
| `FacetTokenizer`, `EmptyTokenizer` | facet paths; nothing | `src/tokenizer/facet_tokenizer.rs`, `empty_tokenizer.rs` |

Pre-registered names (`src/tokenizer/tokenizer_manager.rs:56-79`): `raw`; `default` = Simple + `RemoveLongFilter::limit(40)` + `LowerCaser`; `en_stem` = default + English stemmer (feature `stemmer`); `whitespace`.

## Built-in filters
| Filter | Behaviour | Source |
|---|---|---|
| `LowerCaser` | ASCII fast path, else `char::to_lowercase` | `src/tokenizer/lower_caser.rs:44-63` |
| `AsciiFoldingFilter` | folds non-ASCII letters, digits and symbols to an ASCII equivalent if one exists | `src/tokenizer/ascii_folding_filter.rs:1-3` |
| `RemoveLongFilter::limit(n)` | keeps a token only if `text.len() < n` BYTES; longer tokens vanish | `src/tokenizer/remove_long.rs:29-36` |
| `Stemmer::new(Language)` | Snowball stemmers for 18 languages, feature `stemmer` (moved behind it in 0.26, CHANGELOG.md:44) | `src/tokenizer/stemmer.rs:12-31, 69` |
| `StopWordFilter::new(Language)` / `::remove(words)` | drops listed words, feature `stopwords` | `src/tokenizer/stop_word_filter/mod.rs:37, 67` |
| `SplitCompoundWords::from_dictionary(words)` | splits a token only if it "can be fully decomposed into consecutive matches into the given dictionary" | `src/tokenizer/split_compound_words.rs:1-5, 56` |
| `AlphaNumOnlyFilter` | drops tokens that are not all ASCII alphanumeric | `src/tokenizer/alphanum_only.rs` |

There is no built-in camelCase or snake_case splitter. `SplitCompoundWords` is dictionary-driven, so it cannot split arbitrary identifiers. A code-aware analyzer must be a custom `Tokenizer` or `TokenFilter` (see 18). the bench's Rust arm already has one: `unicode61::Code`, which emits every unicode61 token and, when its spelling has two or more camelCase parts, each folded part at the same position (the bench's Rust arm `src/unicode61.rs:186-191`).

## Query side
- `QueryParser` tokenizes each literal with the field's analyzer; a literal that yields more than one token becomes a PHRASE, not an OR (`src/query/query_parser/query_parser.rs:958-995`). Example: the unquoted query `foo.bar` is the phrase "foo bar". codectx's `encodeFTS` makes unquoted multi-token text an OR of single terms (codectx `internal/search/lexical.go:297-308, 358-364`), so the parser is not a drop-in for codectx's matching; the bench's Rust arm builds queries by hand for that reason.
- Stacked tokens are kept at their position by the parser (`terms.push((token.position, term))`, `query_parser.rs:971`), so a phrase over a code-aware field matches a part at the whole token's position.

## What it means for codectx
- Drop-in traps: `default` silently removes every token of 40 bytes or more (long identifiers, hashes, paths) and lowercases with Unicode rules that differ from unicode61's folding. Neither matches FTS5 unicode61, which is why the bench's Rust arm ships its own `Unicode61` tokenizer (0 differences on 24 002 smoke strings, per the the bench's Rust arm report).
- Stemmer, StopWord, AsciiFolding: "no" for the product arm (they change hit counts relative to FTS5, and unicode61 already removes diacritics). They are only candidates for a separate quality experiment.
- Ngram (e.g. 3-grams on `name`): "maybe" for substring matching of identifiers; large disk and index-time cost. Variant: an extra `name_ngram` field, compare disk and recall on partial-identifier queries.
- The code-aware tokenizer (custom) is the only tokenizer change with a plausible quality gain (see 18).
