# 07 — Quality evaluation: known-item tests built from the export

There are no judged queries, so `tqrels <docs.jsonl> <outdir> <seed>` derives known-item sets from the export itself. Every arm is then scored from the result lines it already writes. The scoring uses the document-level `page`, the search keys in served order.

**Population.** Tree-sitter declaration documents (unit `treesitter/…`) of kinds function, method, struct, class, interface, type, enum and trait, with a non-empty name. They are ordered by search key and shuffled with a xorshift generator seeded by `<seed>`, so the same seed always gives the same sets. Each set takes up to 1 000 distinct queries in shuffle order. Queries containing `"`, a tab or a newline, or yielding no unicode61 token, are skipped.

**Sets**

| set | query | relevant | measures |
|---|---|---|---|
| `name` | the declaration's name exactly | every document whose `name` equals it | exact-identifier lookup |
| `doc` | the 4 lowest-df of the body's distinct unicode61 tokens that are not tokens of the name, path or qualified name (at least 6 required; df over all five columns of the export; ties by bytes) | that one declaration | concept search over attached documentation |
| `subword` | one camelCase/snake_case part of 4+ characters, picked by the generator, lower-cased, from names with at least 2 parts | every declaration document whose name has that part as a whole part | partial-identifier search |

Output: `queries-<set>.txt` (one query per line) and `qrels-<set>.tsv` (`query<TAB>relevant search key`, one line per key).

**Scoring.** `teval <qrels> <queries> <results.jsonl>…` groups lines by (arm, phase) and takes the run-0 line of each query. It prints:
- MRR@10;
- Success@1 and Success@10;
- Recall@200 = relevant keys in the first 200 / all relevant keys;
- mean hits;
- the number of zero-hit queries;
- missing queries, scored as misses.

Keys not in the qrels count as not relevant.

**Reading the subword set.** unicode61 cannot match a camelCase part (`url` does not match `parseURL`), so the set measures a tokenizer capability, not a defect of either arm. snake_case parts are an exception: unicode61 already treats `_` as a separator, so `proxy` matches `kube_proxy` in every arm. The fair contender is the code tokenizer variant (`tindex --tokenizer code`, library modes). It emits each camelCase part as an additional token at the position of its whole token, and query terms gain the same parts.

**Smoke** (synthetic fixture, seed 7: 232 name, 520 doc and 41 subword queries; the vocabulary is tiny and artificial, so these numbers only show that the pipeline works, not anything about quality):
- subword MRR@10: library 0.69, library code 0.73, product 0.76.
- subword R@200: library 0.56, library code 0.97, product 0.56.
- name MRR@10: library 0.96, product 0.96.
