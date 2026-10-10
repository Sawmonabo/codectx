# 08 — Ranking quality for code search: which signals beyond BM25 measurably help

**Question.** Which signals beyond BM25 measurably help code search? The candidates are symbol-kind boosts, name and definition matches, path and test-file demotion, and proximity. Only credible sources are used.

**Credibility key.** A = peer-reviewed paper, or the primary source code of the engine. B = official documentation, or an engineering write-up by the team that runs the system. B* = primary research that is not peer-reviewed. C = secondary, used only to locate a source. Each signal below is marked **measured** (an evaluation with numbers was published) or **asserted** (shipped or recommended without published numbers).

## 1. Short answer

- Every production code-search team boosts **symbol definitions and file-name matches** above plain content matches, and most **demote test, vendored and generated files**. Sources: Google, GitHub, Sourcegraph Zoekt.
- Almost none of this is published as a controlled measurement. The engines publish their constants and designs, not ablations.
- The only published numbers are:
  - one peer-reviewed study that measured text plus code-specific heuristics plus a graph rank **together** (AUC 0.736 → 0.841, 25 queries);
  - one peer-reviewed learning-to-rank ablation in which a **PageRank feature contributed nothing** (0.00% change);
  - Sourcegraph's unpublished internal evaluation: BM25F (symbol and file-name tf ×5) gave "roughly 20% improvement across all key metrics".
- **Proximity** is listed as a candidate signal (Zoekt design) but has no code-search measurement. **Symbol-kind** boosts exist only as tuned constants.

## 2. Findings by signal

### 2.1 Name, symbol-definition and file-name matches

1. **Google Code Search** gives symbol and file-name matches a higher score than ordinary content matches: "The scoring boosts symbol and filename matches over normal content matches to reflect the inferred intent of the developer". It also boosts "results where much of the query occurs in the full path". A fixed retrieval cap "might stop before the file with the single definition was reached", so Google adds supplemental queries restricted to "only definitions and filenames". **Asserted.**
   Source: Neubeck, St. John, "Code Search", ch. 17 of *Software Engineering at Google*, O'Reilly 2020, https://abseil.io/resources/swe-book/html/ch17.html. **B**: the team running Google's Code Search.

2. **GitHub Blackbird** uses code-specific heuristics, including "ranking up definitions and penalizing test code". It ranks complete matches over partial ones (`thread` > `thread_id` > `pthread_getname_np`), and ranks repositories by popularity ("popular open-source repositories before a random match"). **Asserted.**
   Source: Avgustinov (GitHub), "A brief history of code search at GitHub", 2021-12-15, https://github.blog/engineering/architecture-optimization/a-brief-history-of-code-search-at-github/. **B**.

3. **Zoekt's default scorer** (source at commit c8b36a0, 2026-10-07) adds constants per matching line. **Asserted** (tuned constants, no published ablation):

   | Signal | Score |
   |---|---|
   | whole file-name base match | 7000 |
   | partial file-name match | 4000 |
   | match exactly on a ctags symbol | 7000 |
   | partial symbol match | 4000 |
   | word-boundary match | 500 |
   | partial-word match | 50 |
   | symbol kind | up to 100 × factor |
   | more than one query atom matched | up to 400, as (1 − 1/n) × 400 |

   The comment bounds the query-dependent total at about 9000. Ties then break on repository rank and on the document's position in the index.
   Source: `index/contentprovider.go` (const block `scoreSymbol = 7000.0` …) and `index/score.go` (`scoreChunk`, `scoreFile`), https://github.com/sourcegraph/zoekt. **A**: the engine source.

4. **Sourcegraph's BM25F mode** counts a term matched in a file name or a symbol definition five times (`termFreqs[term] += importantTermBoost`, `importantTermBoost = 5`). It divides tf by 5 for "low priority" files: test, vendored, generated and binary (`lowPriorityFilePenalty = 5`, `FileCategory.lowPriority()`). The post reports that BM25 "showed roughly 20% improvement across all key metrics compared to our baseline ranking". The weight 5 came from "a tiny grid of options", and ranking is "not very sensitive" to the exact value. Naively summing per-field scores "gives too big an advantage to files with a match on multiple fields". **Measured internally; the data, metrics and sample size are not published.**
   Source: Tibshirani (Sourcegraph), "Keeping it boring (and relevant) with BM25F", 2025-04-04, https://sourcegraph.com/blog/keeping-it-boring-and-relevant-with-bm25f; and `index/score.go` (`calculateTermFrequency`, `scoreFileBM25`), `index/file_category.go`. **B** for the post, **A** for the code.

5. **Zoekt's BM25 treats idf as constant**: "This is supported by our evaluations which showed that for keyword style queries, idf can down-weight the score of some keywords too much, leading to a worse ranking." **Measured internally, not published.** The BM25 mode is opt-in (`UseBM25Scoring`) and ignores the other signals and document ranks.
   Source: `index/score.go` comment on `scoreFileBM25`; `api.go` `UseBM25Scoring`. **A** for what the code does, **B** for the evaluation claim.

6. **Combined text, code-specific heuristics and graph rank (Sourcerer):**
   - Heuristics: "keyword hits to the right of the fully-qualified name can be boosted, hits in comments can be discounted, and terms indicative of test articles can be ignored", plus CodeRank (PageRank over class and method relations).
   - 25 control queries, judged by 3 engineers, scored with mean AUC:
     - Google web search 0.31;
     - Google Code Search about 0.66;
     - Sourcerer with a Java keyword and comment parser 0.736;
     - plus the heuristics and CodeRank **0.841**.
   - **Measured, but as a bundle:** the heuristics and CodeRank are not separated in the paper. The authors mention 12 schemes in "supplementary materials" that were not reachable.
   Source: Linstead, Rigor, Bajracharya, Lopes, Baldi, "Mining Internet-Scale Software Repositories", NeurIPS 2007, https://papers.nips.cc/paper/2007/file/a532400ed62e772b9dc0b86f46e583ff-Paper.pdf (full text read, §5). **A**: peer-reviewed. Caveats: small query set, Java only, and component reuse rather than navigation queries.

7. **Learning to rank code examples (Android, 360,000 snippets, 50 queries, 2,500 judged examples).** Randomizing a feature tested its contribution to NDCG@10:
   - textual similarity: −21.7% (p = 0.0098);
   - call-sequence frequency: −13.0% (p = 0.027);
   - context similarity (signature against the query's surrounding method): −13.0% (p = 0.065);
   - **PageRank, line length, comment ratio and identifier density: about 0% and not significant** (PageRank −0.00%, p = 1.00).
   **Measured.**
   Source: Niu, Keivanloo, Zou, "Learning to rank code examples for code search engines", Empirical Software Engineering 22(1):259–291, 2017, DOI 10.1007/s10664-015-9421-5; author copy https://seal-queensu.github.io/publications/pdf/EMSE-Haoran-2017.pdf (Table 9 read). **A**: peer-reviewed EMSE. Caveat: code-*example* recommendation from natural-language queries, not symbol navigation.

### 2.2 Symbol-kind boosts

8. **Zoekt ranks kinds** with a generic table:

   | Kind | Factor |
   |---|---|
   | class | 10 |
   | struct | 9.5 |
   | enum | 9 |
   | interface | 8 |
   | function, method | 7 |
   | field | 5.5 |
   | constant | 5 |
   | variable | 4 |
   | other | 1 |

   Per-language tables override it. Go adds +0.5 for exported symbols and multiplies the factor by 0.8 in `_test.go` files. The factor is multiplied by `scoreKindMatch = 100`. **Asserted**: tuned constants, no evaluation published.
   Source: `index/contentprovider.go` `scoreSymbolKind`, https://github.com/sourcegraph/zoekt. **A** for the behaviour.

### 2.3 Path, test-file and generated-file demotion

9. Production systems that demote these files:
   - GitHub penalizes test code (#2).
   - Zoekt divides tf by 5 for test, vendored, generated and binary files in BM25 mode (#4), and multiplies by 0.8 for Go test symbols (#8).
   - Sourcegraph down-ranks generated code ("usually the least interesting in results"), vendored code ("Developers are normally looking for code written by their organisation") and test code ("Developers normally prefer results in non-test code over test code"). It orders repositories by star count, with admin overrides, and calls this "an area of active research and is subject to change". Source: Sourcegraph, "Indexed ranking", developer docs (5.x), https://4.5.sourcegraph.com/dev/background-information/architecture/indexed-ranking (read). **B**.
   - Sourcerer ignored "terms indicative of test articles" (#6, measured only as part of the bundle).

   **No isolated measurement found.**

10. **Path as a positive signal:** Google boosts the query occurring in the full path (#1). Developers scope by path heavily: 26.5% of 3,870 Google queries used `file:`. Source: Sadowski, Stolee, Elbaum, "How Developers Search for Code: A Case Study", ESEC/FSE 2015, https://research.google/pubs/pub43835/. **A**. That is evidence for path *filters*, not for path *scoring*.

### 2.4 Graph and popularity signals

11. Google Code Search uses "the number of file views and the amount of references to a file", in PageRank style with include and import statements as links (#1, **asserted**). Sourcegraph ran PageRank over the file reference graph and chose an undirected graph, because directed edges over-ranked generated files. "Hand-vetted" only, **no metrics**. Source: Fritz (Sourcegraph), "Ranking in a week", 2022-11-21, https://sourcegraph.com/blog/ranking-in-a-week. **B**. The one ablation found shows **no gain** from PageRank (#7).

### 2.5 Proximity

12. Zoekt's design document lists "closeness to matches for other atoms" as a candidate signal. Source: https://github.com/sourcegraph/zoekt/blob/main/doc/design.md. **B**, **asserted**. The shipped scorer has no proximity term; it scores by line or chunk instead.
    In general (non-code) IR, Tao and Zhai report that adding a term-proximity measure to BM25 improves results significantly on five TREC collections. Source: "An Exploration of Proximity Measures in Information Retrieval", SIGIR 2007, pp. 295–302, DOI 10.1145/1277741.1277794. **A** venue, but the full text was not reachable (ACM returned 403, author host refused), so the claim rests on the abstract as summarized. **No code-search measurement of proximity was found.**

### 2.6 Sources considered and rejected

- searchcode.com, "Code Search Ranking: A Benchmark Across 4 Tools and 41 Queries" (Boyter, 2026). Rejected: the vendor ranks its own engine, the competitors are anonymized, no per-signal ablation is given, the headline and body figures disagree, and the queries are not published.
- GitHub pull requests in small AI-assistant tooling projects that report MRR changes from test demotion. Rejected: not peer-reviewed, tiny query sets, tuned on the test queries.

## 3. Measured or asserted, per signal

| Signal | Who uses it | Evidence |
|---|---|---|
| Symbol-definition match boost | Google, GitHub, Zoekt, Sourcegraph BM25F | asserted. Sourcegraph internal "about 20%" for BM25F as a whole (not published). Sourcerer bundle measured (0.736 → 0.841) |
| File-name match boost | Google, Zoekt (7000), Sourcegraph BM25F (×5) | asserted / internal only |
| Whole-token over partial match | GitHub, Google, Zoekt (500 vs 50) | asserted |
| Symbol-kind ranking | Zoekt tables | asserted |
| Test, vendor and generated demotion | GitHub, Zoekt (÷5, ×0.8), Sourcegraph | asserted. Sourcerer bundle only |
| Path contains query (scoring) | Google | asserted |
| Path scoping (filter) | Google users, 26.5% of queries | measured (usage, not quality) |
| PageRank / reference graph | Google, Sourcegraph | asserted. **Measured null** in one LTR ablation (EMSE 2017) |
| Constant idf (no idf) | Zoekt BM25 mode | internal evaluation claimed, not published |
| Proximity | Zoekt design (candidate only) | not measured for code; general IR only (SIGIR 2007) |
| Query-term coverage (atoms matched) | Zoekt (≤ 400) | asserted |

**Evidence is thin where:** no public, judged code-search test collection reports ablations of definition boosts, kind boosts, test demotion or proximity. Every production team cites internal evaluations whose data is unpublished. The two peer-reviewed measurements are small (25 and 50 queries) and use natural-language or reuse queries. Neither uses identifier navigation, which is codectx's main query type.

## 4. What this means for codectx

- **Rule 9 (tiers first: path, exact qualified name, qualified-name prefix, exact name; then lexical score): supported** by practice everywhere, and by Google's explicit concern that a capped retrieval misses "the single definition". Zoekt reaches a similar effect with dominant constants (7000 for a symbol or file-name match, against 500 for a word match), so a symbol match nearly always wins. Unmeasured, but no source contradicts it.
- **Rule 7 (BM25F with one saturation, weights 5/5/2/2/1): supported in form.** Sourcegraph moved from per-field summing to BM25F, with a weight of 5 on symbol and file-name tf, and reports an internal gain. It notes that per-field summing over-rewards multi-field matches. The exact weights 5/5/2/2/1 have no published grounding, and Sourcegraph says ranking is not very sensitive to the value. **Gap:** codectx applies no test, vendor or generated demotion inside BM25F. Every production system does, but no measurement exists. A candidate is a low-priority-path tf divisor, Zoekt's ÷5, adopted only after a judged evaluation.
- **Rule 1 (exact idf over the column union): neutral to mildly contradicted.** Zoekt's evaluation claim is that idf hurts keyword-style code queries. It is unpublished, and on its own it is not strong enough to drop idf. It is worth one ablation (constant idf against rule 1 idf) on codectx's judged set.
- **Rule 8 (tokenizer):** see file 06. Whole-token over partial-match preference is universal, which argues for keeping whole identifiers as tokens.
- **Symbol-kind boosts:** codectx has none in the lexical score, and only `kind` filters. The evidence is asserted constants only, so there is no basis for adopting them before measurement.
- **Graph or popularity rank:** not supported as a priority. The one ablation found no gain, and the production uses are unmeasured.
- **Proximity:** no code evidence. codectx's phrase terms already need positions, so it is a cheap experiment, not a rule.
- **Rule 2, rule 3, rule 4, rule 5, rule 6, rule 10, rule 11:** neutral. Ranking signals do not bear on them, with two notes:
  - any query-independent signal must be content-derived and stored per document, or rule 2 (delta = fresh) breaks;
  - Zoekt's tie-break on index position is exactly what rule 4 forbids.
