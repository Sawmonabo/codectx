# 01 — Reproducing SQLite unicode61 for tantivy: the probe and its result

**Question.** Our engine tokenizes every search column and every query with SQLite FTS5's `unicode61` tokenizer, using its defaults (categories `L* N* Co`, `remove_diacritics=1`). A tantivy index can only be compared like for like if its tokenizer produces the same terms at the same positions.

**Method: probe the exact build, don't port from memory.** `source/probes/tokgen/main.go` uses the same SQLite driver and version the product links (it reports SQLite 3.53.4). It creates `fts5(name, qualified_name, signature, path, body, tokenize='unicode61', detail='full')`, the same declaration the product's tokenizer connection uses. For every Unicode scalar value `c` (surrogates excluded) it inserts the document `"a" + c + "b"` and reads the token instances back through `fts5vocab(…, 'instance')`.
- Two tokens `a`, `b` → `c` separates tokens.
- One token `a X b` → `c` is a token character that folds to `X`, where `X` may be empty (a diacritic that is removed).

Every codepoint fell into exactly one of these classes: **0 anomalies, 410 separator ranges, 1 243 folds** (each fold is at most one codepoint). The generator writes these tables as `source/src/unicode61_tables.rs`. `source/src/unicode61.rs` turns them into a tokenizer: a token is a maximal run of non-separators, and each codepoint is folded on its own.

**Behaviours found only by probing, and now reproduced:**
1. A run whose every codepoint folds away (for example a lone U+0301) is **not a token and takes no position**. The first diff showed an extra empty token here, and the fix closed it.
2. FTS5 truncates a token to **32 768 bytes** (FTS5_MAX_TOKEN_SIZE): a 40 000-byte token came back at 32 768 bytes.

**Cross-check on text.** `tokgen tok` (SQLite) and `ttok` (Rust) both print `(position, hex term)` for every string of a JSONL file. Over the 24 002 column strings of the smoke fixture plus edge strings (303 391 tokens: mixed case, digits, accents, CJK, ligatures, dotted İ, final sigma, full-width, circled digits, private use, zero-width space, emoji), the outputs are byte-identical except on 2 lines. Those 2 lines are the one known departure.

**The departure.** When FTS5's 32 768-byte cut falls inside a multi-byte character, SQLite keeps the partial bytes, giving an invalid UTF-8 term. A tantivy token is a Rust `String`, so the Rust side cuts at the previous character boundary instead. Queries go through the same tokenizer and are cut the same way, so matching is equivalent; only that term's stored bytes differ. tantivy's own limit is larger: it drops tokens over `MAX_TOKEN_LEN` = 65 530 bytes (`tantivy-0.26.2/src/tokenizer/mod.rs:168`, `src/postings/postings_writer.rs:141`), so it never interferes.

**Also checked by every index build.** `tindex` recounts each document's tokens over the five columns and compares the count with the contract's `token_count`, our store's value. The smoke reported 0 mismatches.
