# 18 — A quality evaluation the library can support: code-aware tokenization and scoring variants

Version: tantivy 0.26.2. Paths are inside `tantivy-0.26.2/` unless they name the bench's own harness (`rust/src/...` in the bench workspace).

## Why tokenization is the lever
codectx tokenizes with SQLite FTS5 unicode61: `getUserName` is one token, `get_user_name` is three (`_` separates). A user who types `user name` or `username` never reaches `getUserName` lexically. tantivy cannot change BM25 itself (fixed K1/B, `src/query/bm25.rs:8-9`), but it accepts any custom `Tokenizer`/`TokenFilter` (`tantivy-tokenizer-api-0.7.0/src/lib.rs:56, 152`), stacks tokens that share a `position` (`src/postings/postings_writer.rs:152-154`), and lets several fields carry different analyses of the same text, as long as the caller feeds each field (ARCHITECTURE.md:118-122). Those three facts are all a code-aware variant needs.

## The variant that already exists
The bench's Rust arm has a `Code` tokenizer. It emits every unicode61 token and, when a token's spelling has at least two camelCase parts, each folded part at the SAME position (`rust/src/unicode61.rs:186-191`). Part rules (`:98-131`): a part starts at an upper-case letter after a lower-case letter or a digit, and at the last capital of an acronym followed by a lower-case letter ("HTTPClient" -> "HTTP", "Client"); digits stay with the preceding part ("v1beta1" is one part). `tindex --tokenizer code` builds it; library modes expand query terms with their parts; product mode refuses such an index, because codectx's matching has no parts.

## Design: three index variants from the same `docs.jsonl`
1. **Baseline**: unicode61 on all five columns (today's index).
2. **Stacked**: `code` on all five columns (exists). Side effect: stacked parts count in the fieldnorm (`src/postings/postings_writer.rs:155`; `src/indexer/segment_writer.rs:218-220`), so documents with many identifiers look longer to BM25.
3. **Side fields** (new): baseline fields unchanged, plus `name_parts` and `qualified_name_parts` analyzed with a parts-only analyzer (parts of multi-part identifiers, nothing else), each with its own boost (e.g. 2). Baseline fieldnorms and statistics are untouched, and part matches score separately.

## Query bands
- The 30 contract queries (`queries.txt`), unchanged. Expected: hits on the baseline and the side-field variant are a superset of the baseline's; any LOST hit is a defect.
- A new identifier band, built from `docs.jsonl` `name` values with at least two camelCase parts, sampled across df (5 frequent, 5 mid, 5 rare). For each identifier derive four queries: the whole identifier (`getUserName`), one inner part (`user`), the parts as words (`user name`), and the parts as a phrase (`"user name"`). Phrases need the query side to place parts at consecutive positions (`PhraseQuery::new_with_offset`, `src/query/phrase_query/phrase_query.rs:43`).

## Judgments without human labels (known-item retrieval)
For each identifier query the target set is the documents whose `name` equals the identifier (read from the `name_s` fast field). Metrics per variant and band: success@1, @10 and @200 and the reciprocal rank of the best target in the served page; hits (complete count); page overlap with the baseline (Jaccard over the 200 ids and rank-biased overlap); and, for the contract queries, the count of hits lost. Cost metrics: index wall and CPU, peak RSS, disk by component from `Searcher::space_usage()` (`src/core/searcher.rs:240`), and warm and cold latency.

## Scoring variants judged the same way
Each is a variant against the same targets and against product mode's page (product mode is codectx's own answer):
- Library nested SHOULD (today) vs flat `SHOULD[Boost(Term)]` (block-WAND eligible, `src/query/boolean_query/boolean_weight.rs:57-68`).
- `DisjunctionMaxQuery` per term over the five boosted fields, tie_breaker 0.1 and 0.3, collected without pruning (the TopDocs path sums instead of taking the max, see 12).
- Library BM25 with an alive-statistics provider vs the default provider, after the delta (see 13).
- Fieldnorms off on all five fields (constant norm, `src/query/term_query/term_weight.rs:209-217`).

## Guardrails
- Every variant's ranking uses a deterministic tie-break (`(score desc, id asc)`, verified in 06) so overlap metrics compare rankings, not tie noise.
- One capped workload at a time, as in the bench rules; report the exact commands.
- The quality variants inform a product decision; they are not a substitute for product mode's exactness checks (`tcheck`).
