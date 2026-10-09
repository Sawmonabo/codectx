# 06 — Extension points used by product mode, and why the stock ones could not reproduce our scoring

Our lexical scoring is BM25F with:
- one term frequency per term, summed across columns with weights 5/5/2/2/1 before saturation;
- df counted per term over the union of all five columns, on live documents only;
- the document length taken from `token_count`, not per field;
- a score computed in f64, quantized to 1e-6 and ordered on integers by a total comparator.

Two of tantivy's stock pieces cannot express this.
- **`Bm25StatisticsProvider`** (`tantivy-0.26.2/src/query/bm25.rs:15-24`) answers `doc_freq(term)` for a Term, which belongs to one field, and `total_num_tokens(field)` per field. It has no cross-field df. Its searcher implementation counts deleted documents: `total_num_docs` sums `max_doc` (`src/query/bm25.rs:38-44`), and `doc_freq` sums the segment term infos (`src/core/searcher.rs:133-141`; `InvertedIndexReader::total_num_tokens` likewise includes deleted documents, `src/index/inverted_index_reader.rs:249-251`).
- **`Score` is `f32`** (`src/lib.rs:314`). Any `Scorer` or `Weight` hands a collector an f32, which cannot carry our f64 value to an exact 1e-6 quantization.

**What product mode uses** (`source/src/product.rs`):
1. **A custom `Collector` that overrides `collect_segment`** (`src/collector/mod.rs:141,173`). The default body calls the Weight's scorer; the override ignores the Weight (the search is issued with `AllQuery` and `requires_scoring() = false`) and walks the segment itself, once per segment through `Searcher::search` (`src/core/searcher.rs:180`).
2. **`InvertedIndexReader::read_postings`** (`src/index/inverted_index_reader.rs:265`):
   - Single tokens are read with freqs, accumulating `Σ weight·tf` per document.
   - Phrases are read with positions (`Postings::positions`, `src/postings/postings.rs:31`). The cursors are intersected with `seek`, and only when a cursor is behind, because `SegmentPostings::seek` debug-asserts a forward target (`src/postings/segment_postings.rs:172-173`). Each token's positions are truncated to 512 per (document, column), and adjacent starts are counted exactly as our `phraseCount` does.
3. **The segment's alive bitset** (`src/index/segment_reader.rs:433`). It makes df, N and the token total exact over live documents, after deletes too.
4. **Fast-field columns:**
   - `token_count` gives the document length.
   - `start` and `end` give the byte range.
   - String columns give path, kind, name, qualified name, signature, node id and id.
   - Inside one segment, the comparator and the fold run on dictionary ordinals, which order exactly as the bytes do.
   - Only each segment's fold survivors are resolved to strings, in one sorted pass per column with `Dictionary::sorted_ords_to_term_cb` (`tantivy-sstable-0.7.0/src/dictionary.rs:515`).
   - Filters become ordinal sets and ranges, through `Dictionary::term_ord` (`…/dictionary.rs:418`) and a `range().ge(P + "/")` stream.
5. **`merge_fruits`** computes the global df, IDF and scores. It folds across segments with full strings, then selects and sorts the page with cmpScored.

**library-all** uses a second custom collector, with `requires_scoring() = true` and a segment collector that keeps every `(doc, score)`. It changes only collection, not tantivy's scoring.
