# 06 — Code-aware tokenization: does splitting identifiers improve code search?

**Question.** Is there evidence that splitting camelCase and snake_case identifiers, and similar code-specific analysis, improves code search? What do production code-search systems actually do?

**Credibility key.** A = peer-reviewed paper, or the primary source code of the engine. B = official documentation, or an engineering write-up by the team that runs the system. B* = primary research that is not peer-reviewed (arXiv preprint, technical report); weigh it like B. C = secondary; used only to locate an A/B source and never cited for a claim. Each claim says whether the effect was **measured** or only **asserted**.

## 1. Short answer

- Splitting identifiers into words is near-universal in software-engineering IR research, and one recent ablation measures a large BM25 gain from it on natural-language queries. The best-measured gain comes from **adding the parts while keeping the whole identifier**, not from replacing the identifier with its parts.
- Going from plain camelCase splitting to a smarter splitter (Samurai, or even a perfect manual oracle) gave little or no retrieval gain in the one controlled study that tested it.
- Production systems that serve identifier-style queries (GitHub, Google, Sourcegraph Zoekt, livegrep) mostly gave up word tokenization in favour of **substring matching** (trigrams, sparse n-grams, suffix arrays). They then rank whole-token and symbol matches highest. Systems that do tokenize (GitHub's old Elasticsearch index, OpenGrok) kept the whole identifier as a token.
- No study measures codectx's exact choice, `unicode61`: it splits on underscore and drops the whole snake_case form, and it does not split camelCase.

## 2. Findings

### 2.1 Identifier splitting accuracy (the input to every later step)

1. **Samurai splits identifiers by mining word frequencies from source code. It beat the earlier splitters on more than 8,000 Java identifiers.** Measured, as splitting accuracy against a manual oracle.
   Source: Enslen, Hill, Pollock, Vijay-Shanker, "Mining Source Code to Automatically Split Identifiers for Software Analysis", MSR 2009, pp. 71–80, DOI 10.1109/MSR.2009.5069482, https://flosshub.org/node/928 (bibliographic record). **A**: peer-reviewed MSR paper. The full text was not read; the abstract claim was seen in the record. Samurai's accuracy is independently confirmed by the EMSE 2014 study (#2).

2. **Conservative camelCase-plus-delimiter splitting is almost never wrong when it splits, but it misses same-case joins.** On a 2,733-identifier oracle (C, C++, Java), the conservative splitter ("CS") scored mean per-identifier accuracy 0.71, precision 1.00 and recall 0.90. Samurai scored 0.82, precision 0.97 and recall 0.96 (Table 7). 25% of the identifiers were split correctly by every technique, and these "predominantly involve camel case, underscore". All 43 identifiers that no technique split correctly were same-case joins (`strdup`, `COMPUTIME`). Measured.
   Source: Hill, Binkley, Lawrie, Pollock, Vijay-Shanker, "An Empirical Study of Identifier Splitting Techniques", Empirical Software Engineering 19(6):1754–1780, 2014, DOI 10.1007/s10664-013-9261-0; author copy https://www.eecis.udel.edu/~pollock/879tainsef13/idsplittjournal.pdf (full text read). **A**: peer-reviewed EMSE journal. It measures splitting accuracy, not search quality.

3. **LINSEN** splits and expands abbreviations with a graph model in time linear in the dictionary size. Its authors report better splitting and expansion than earlier techniques on oracles drawn from 24 C/C++ and Java systems. Measured (splitting only).
   Source: Corazza, Di Martino, Maggio, "LINSEN: An efficient approach to split identifiers and expand abbreviations", ICSM 2012, pp. 233–242, DOI 10.1109/ICSM.2012.6405277. **A** for existence and claim; only secondary summaries were reachable (Semantic Scholar record), so the claim is not verified from the full text.

4. **Spiral** is a Python package of splitters: naive camelCase splitters, Samurai, and Ronin (heuristics plus token frequencies mined from more than 46,000 GitHub projects). Its own accuracy figures are on the Ludiso and INTT oracles. Measured (splitting only).
   Source: Hucka, "Spiral: splitters for identifiers in source code files", Journal of Open Source Software 3(24):653, 2018, https://joss.theoj.org/papers/10.21105/joss.00653, and the README at https://github.com/casics/spiral. **B**: JOSS reviews software, not results.

### 2.2 Does splitting change retrieval quality? (the evidence that matters)

5. **A better splitter than camelCase rarely helps feature location, and a perfect one only sometimes.** The study used two feature-location techniques (LSI-based IR, and IR plus execution traces) on Rhino and jEdit (four datasets). It compared CamelCase splitting with Samurai and with a manual oracle. Wilcoxon tests against CamelCase: Samurai was never significant (p from 0.479 to 0.890). The oracle was significant on one of the four datasets (RhinoFeatures, p = 0.005), with median effectiveness rank 23 → 20 and the mean unchanged at 86. With execution data, no splitter beat any other. On jEditBugs the oracle did *worse* than CamelCase by percentages. Measured, but on small datasets of a few hundred queries in total.
   Source: Dit, Guerrouj, Poshyvanyk, Antoniol, "Can Better Identifier Splitting Techniques Help Feature Location?", ICPC 2011, pp. 11–20, DOI 10.1109/ICPC.2011.47, https://www.cs.wm.edu/~denys/pubs/ICPC2011-IdentifierSplitting.pdf (full text read). **A**: peer-reviewed ICPC paper. Caveat: the baseline already splits camelCase. This paper says nothing about splitting against *not* splitting.

6. **Splitting while keeping the whole identifier is the largest single BM25 gain measured on code. Splitting into parts only is worse, and on Python whole identifiers win.** BM25 (bm25s, k1 = 1.5, b = 0.75) was run on CoIR CodeSearchNet, 50,000 documents per language, with NDCG@10 as the measure:
   - **Go:** T0 (default regex `\b\w\w+\b`, lowercase, stopwords, which keeps snake_case whole and does not split camelCase) 0.309. T1 (whitespace) 0.149. **T2 (whole identifier plus camelCase/snake_case parts) 0.563**, +82%. T3 (parts only) 0.469.
   - **Java:** T2 was 27.8% over T0. **Ruby:** 22.5%.
   - **Python** (snake_case): T2 gave +0.3% and T3 +0.0%, while whitespace-only T1 gave **+17.5%** over T0. The authors: "the code-aware tokenizer that decomposes snake_case reduces signal".
   - The authors recommend matching the analyzer to each language's identifier style.
   Measured, with 95% paired-bootstrap confidence intervals for the main results.
   Source: Radha, Goktas, "Improving BM25 Code Retrieval Under Fixed Generic Tokenization: Adaptive q-Log Odds as a Drop-In BM25 Fix", arXiv:2605.18561, May 2026, https://arxiv.org/abs/2605.18561 (full text read, section 7 and Table 5). **B***: a recent preprint from one group, on one benchmark family. The queries are natural-language docstrings, not identifier queries. Not peer-reviewed or replicated yet.

7. **CodeSearchNet's lexical baseline already split function names into subtokens and was "competitive".** The Elasticsearch baseline indexed two fields, "the function name, split into subtokens; and the text of the entire function", with the default Elasticsearch tokenizer. It scored NDCG (within annotated) 0.337 on average, against 0.574 for the best neural bag-of-words and 0.493 for self-attention. The authors note it "has the advantage of being able to efficiently use rare terms, which often appear in code". The neural models also split identifiers ("camelCase yields two subtokens camel and case"). No ablation of splitting was run. Asserted (no ablation).
   Source: Husain, Wu, Gazit, Allamanis, Brockschmidt, "CodeSearchNet Challenge: Evaluating the State of Semantic Code Search", arXiv:1909.09436, 2019, https://arxiv.org/abs/1909.09436 (full text read, sections 4.2 and 5, Table 4). **B***: widely used technical report from GitHub and Microsoft Research, not peer-reviewed.

8. **What developers type.** At Google, the mean was 1.85 keywords per query (median 1). 26.5% of 3,870 queries used `file:` to scope by path, and 5.4% used `lang:`. Developers search for code very often: a mean of 12 queries per weekday. Measured (logs of 27 developers over 15 days, and a survey with 396 responses).
   Source: Sadowski, Stolee, Elbaum, "How Developers Search for Code: A Case Study", ESEC/FSE 2015, https://research.google/pubs/pub43835/ (full text read). **A**: peer-reviewed FSE paper. It implies that code queries are short and often a single identifier, so how one identifier is tokenized dominates the result.

### 2.3 What production systems do

9. **GitHub, old Elasticsearch index:** a custom pattern tokenizer dropped punctuation ("the special characters simply do not figure in the index"). It added camelCase and snake_case parts **as well as the full identifier** (`pthread_getname_np` → parts plus the whole). Substring queries like `thread_getname` failed. Trigrams were rejected at the time for cost. Asserted (an engineering account, no metrics).
   Source: Avgustinov (GitHub), "A brief history of code search at GitHub", 2021-12-15, https://github.blog/engineering/architecture-optimization/a-brief-history-of-code-search-at-github/. **B**: the team running the system.

10. **GitHub Blackbird (the current engine):** Rust, with n-gram indexes (trigrams and "sparse grams") over content, symbols and paths. It keeps punctuation, does no stemming, removes no stop words, and supports regex. Each shard returns candidates, and the query service merges them and returns the top 100. The 2021 post says ranking favours complete matches over partial ones: `thread` > `thread_id` > `pthread_getname_np`. Asserted.
    Source: Clem (GitHub), "The technology behind GitHub's new code search", 2023-02-06, https://github.blog/engineering/architecture-optimization/the-technology-behind-githubs-new-code-search/. **B**.

11. **Google Code Search:** the index evolved from trigrams to suffix arrays to sparse n-grams. The team rejects word tokenization because it ignores case, stems, and "makes it impossible to search on whitespace or other word delimiters". "Unlike web search, we don't just match on tokens". Clean token matches get "a further boost". Asserted (production figures: about 1.5 TB indexed, about 200 QPS, median latency under 50 ms).
    Source: Neubeck, St. John, "Code Search", chapter 17 of *Software Engineering at Google* (O'Reilly, 2020), https://abseil.io/resources/swe-book/html/ch17.html. **B**: the team that built it.

12. **Google Code Search, public (2006–2012):** a trigram index, because "regular expression matches do not always line up nicely on word boundaries". The index is about 20% of the source size (420 MB of Linux sources gave a 77 MB index).
    Source: Cox, "Regular Expression Matching with a Trigram Index", January 2012, https://swtch.com/~rsc/regexp/regexp4.html. **B**: the author built the system.

13. **Sourcegraph Zoekt:** a positional trigram index. Search is case-insensitive by default ("Code usually is searched without regard for case"). There is no identifier tokenization. Ranking checks whether a match starts and ends on a word boundary, using a byte-class change test (`index/score.go`, the `startBoundary`/`endBoundary` logic). A word match earns 500 and a partial-word match 50 (`index/contentprovider.go`, constants `scoreWordMatch = 500.0` and `scorePartialWordMatch = 50.0`). Measured: no. These are tuned constants in source.
    Source: https://github.com/sourcegraph/zoekt/blob/main/doc/design.md and the source at commit c8b36a0 (2026-10-07). **A** for the code, **B** for the design document.

14. **livegrep:** one suffix array over a flattened buffer of the deduplicated source, searched with RE2 regexes. There is no tokenization and no relevance ranking is described. The index is "usually 3–5x the size of the indexed text".
    Source: Elhage, "Regular expression search with suffix arrays", 2015-02-01, https://blog.nelhage.com/2015/02/regular-expression-search-with-suffix-arrays/, and https://github.com/livegrep/livegrep. **B**.

15. **OpenGrok (on Lucene):** the `full` field tokenizer emits whole identifiers (`[a-zA-Z_][a-zA-Z0-9_]*`, so underscore stays inside the token), numbers and single punctuation characters, all lowercased. There is **no camelCase splitting**. Definitions and references come from ctags into separate `defs` and `refs` fields, and `path` is its own field.
    Source: `opengrok-indexer/src/main/jflex/analysis/plain/PlainFullTokenizer.lex` and `.../search/QueryBuilder.java` at commit e3938b2 (2026-10-08), https://github.com/oracle/opengrok. **A**: the engine source.

16. **Lucene and Elasticsearch analysis:** `word_delimiter_graph` splits on case change, letter-number transitions and non-alphanumerics. Its options are `preserve_original` (default false), which keeps the unsplit token, and `catenate_words`. The documentation warns that multi-position output needs `flatten_graph` at index time, and advises against pairing the filter with the standard tokenizer.
    Source: Elastic, "Word delimiter graph token filter", https://www.elastic.co/docs/reference/text-analysis/analysis-word-delimiter-graph-tokenfilter. **B**.

17. **Bleve** ships a `camelcase` token filter (`analysis/token/camelcase/`, commit 3235617). **tantivy 0.26.2** has no case-transition filter in `src/tokenizer/` (modules: simple, whitespace, regex, ngram, lower_caser, ascii_folding, remove_long, stemmer, stop_word_filter, split_compound_words and others). A code-aware analyzer in tantivy is a custom `TokenFilter`. **A**: the engine sources.

18. **SQLite FTS5 `unicode61`:** token characters are the categories `L* N* Co`, and everything else is a separator. So `_` (category Pc) splits snake_case and **the whole snake_case identifier is not indexed**. Tokens are case-folded, so camelCase becomes one lowercased token. `tokenchars '-_'` is the documented way to keep `_` inside tokens.
    Source: SQLite, "SQLite FTS5 Extension", §4.3.1, https://www.sqlite.org/fts5.html. **B**: official documentation.

## 3. Measured or asserted

| Claim | Status |
|---|---|
| Camel and underscore splitting is precise; same-case joins are the remaining error | measured (EMSE 2014) |
| A better splitter than camelCase improves IR feature location | measured, mostly null (ICPC 2011) |
| Whole identifier plus parts beats generic tokenization for BM25 (Go, Java, Ruby) | measured, one preprint |
| Parts only beats generic tokenization but loses to whole plus parts | measured, one preprint (Go) |
| On snake_case Python, keeping identifiers whole beats splitting | measured, one preprint |
| Substring (n-gram) indexing beats tokenization for code search | asserted (GitHub, Google, Zoekt); no published head-to-head |
| Whole-token or word-boundary matches should outrank partial matches | asserted (GitHub, Google, Zoekt constants) |

**Evidence is thin where:** no peer-reviewed study compares "split" against "not split" for lexical code search with identifier-style queries, the dominant query type at Google (1.85 terms). The only direct splitting ablation for BM25 is one 2026 preprint, on natural-language queries. No study measures FTS5 `unicode61`'s combination: underscore split with the whole form dropped, and no camelCase split. No production system publishes a quality comparison of its tokenization against an alternative.

## 4. What this means for codectx

- **Rule 8 (unicode61: lowercase, split on `_` and punctuation, camelCase not split, no stemming): partly contradicted.**
  - *No stemming:* supported (Blackbird, Google ch. 17).
  - *camelCase not split:* contradicted by the best evidence available. Whole-plus-parts gave +28% (Java) and +82% (Go) NDCG@10 for BM25. With rule 8 as it stands, a query `queue` cannot reach `lastQueueItemPriority` through the lexical tier.
  - *Splitting snake_case without keeping the whole form:* contradicted in direction. Parts-only lost to whole-plus-parts on Go (0.469 vs 0.563). On Python, whole identifiers won. Every tokenizing production system kept the whole identifier (GitHub's Elasticsearch index, OpenGrok).
  - Caveat: the evidence is one preprint on natural-language queries. codectx's tiers rule 9 already serve exact names, which softens the identifier-query case.
  - Candidate rule: emit the whole identifier **and** its case and underscore parts at the same position. In tantivy that is a custom `TokenFilter`; in FTS5, a custom tokenizer.
- **Rule 7 (BM25F with weights 5/5/2/2/1 on name, qualified name, signature, path, body): neutral to supportive.** CodeSearchNet's competitive lexical baseline also used a separate name field split into subtokens. Changing rule 8 changes the df and tf inputs, so rule 7's weights would need re-tuning on a judged set.
- **Rule 1 (df over the column union):** neutral. It is unaffected in principle. The parts add terms with high df, which BM25's idf already discounts.
- **Rule 9 (tiers first: path, exact qualified name, qualified-name prefix, exact name):** supported. Every production system ranks exact and whole-token symbol or file-name matches above partial content matches: Blackbird's "complete matches over partial", Google's "clean token matches" boost, Zoekt's word-boundary and symbol constants.
- **The tantivy decision:** tokenization does not favour tantivy over the current engine, or the reverse. Neither ships a camelCase filter: tantivy needs a custom `TokenFilter`, and FTS5 needs a custom tokenizer. Lucene (`word_delimiter_graph` with `preserve_original`) and Bleve (`camelcase`) ship one.
- **Rule 2, rule 3, rule 4, rule 5, rule 6, rule 10, rule 11:** neutral. Tokenization does not bear on them, except that any change to rule 8 needs a full rebuild so that a delta still equals a fresh index (rule 2).
