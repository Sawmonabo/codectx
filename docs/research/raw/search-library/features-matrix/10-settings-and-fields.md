# 10 — Index settings, Cargo features, and field options

Version: tantivy 0.26.2. Paths are inside `tantivy-0.26.2/` unless a sub-crate is named. Docs: https://docs.rs/tantivy/0.26.2/tantivy/struct.IndexSettings.html, https://docs.rs/tantivy/0.26.2/tantivy/schema/index.html

## IndexSettings (all of it)
`src/index/index_meta.rs:214-241`:
- `docstore_compression: Compressor`, default `Lz4` when the `lz4-compression` feature is on (`src/store/compressors.rs:133-143`).
- `docstore_blocksize: usize`, default 16 384 bytes (`src/index/index_meta.rs:229-232`).
- `docstore_compress_dedicated_thread: bool`, default true, `#[doc(hidden)]` (`:218-223`).
- Nothing else. Index sorting (`sort_by_field`) was removed in 0.24 (CHANGELOG.md:105).
Set it with `Index::builder().settings(IndexSettings{..}).schema(..).create_in_dir(..)` (`src/index/index.rs:128`).

## Docstore compressors
`src/store/compressors.rs:10-19`: `Compressor::None`, `Compressor::Lz4` (feature `lz4-compression`), `Compressor::Zstd(ZstdCompressor { compression_level: Option<i32> })` (feature `zstd-compression`). "The compression level, if unset defaults to zstd::DEFAULT_COMPRESSION_LEVEL = 3" (`:82`). Level 15 and up logs "High compression levels (>=15) are slow and will limit indexing speed" (`:108-113`). Brotli and Snappy were removed in 0.21 (CHANGELOG.md:258).

## Cargo features (they gate options above)
`Cargo.toml:44-72`. The default set is `mmap, stopwords, lz4-compression, columnar-zstd-compression, stemmer`. `zstd-compression` is NOT a default feature: a Zstd docstore variant needs `tantivy = { version = "=0.26.2", features = ["zstd-compression"] }`. Other opt-in features: `quickwit` (sstable term dictionary and async reads, see 16), `compare_hash_only` (stacker: compare term hashes only in the indexing hash map, `Cargo.toml:46`), `failpoints`, `unstable`.

## Docstore reader cache
`IndexReaderBuilder::doc_store_cache_num_blocks(n)`, default `DOCSTORE_CACHE_CAPACITY = 100` decompressed blocks per segment reader (`src/store/reader.rs:25`, `src/reader/mod.rs:57, 118-123`). `Searcher::doc_store_cache_stats()` reports hits and misses (`src/core/searcher.rs:96`). ARCHITECTURE.md: "if you hit the docstore more than 100 times per search query, you are probably misusing tantivy" (ARCHITECTURE.md:159). The the bench's Rust arm library page fetches 200 stored ids per query.

## Field options
- Text: `TextOptions` with `set_indexing_options(TextFieldIndexing)`, `set_stored()`, `set_fast(Option<&str> tokenizer)`, `set_coerce()` (`src/schema/text_options.rs:129-160`). `TextFieldIndexing` has `set_tokenizer`, `set_fieldnorms(bool)` (default true), `set_index_option(IndexRecordOption)` (`:195-258`). Shorthands `TEXT` (default tokenizer, positions) and `STRING` (raw tokenizer, Basic) (`:264-290`).
- `IndexRecordOption::{Basic, WithFreqs, WithFreqsAndPositions}` (`src/schema/index_record_option.rs:19-28`). Freqs are needed for BM25 and block-WAND; positions for phrases. Asking a reader for more than was indexed is downgraded, not an error (CHANGELOG 0.20 "Auto downgrade index record option").
- Numeric/date/bool/ip: `NumericOptions` `set_indexed`, `set_fieldnorm`, `set_fast`, `set_stored`, `set_coerce` (`src/schema/numeric_options.rs:87-131`); flags `INDEXED | STORED | FAST | COERCE` (`src/schema/flags.rs:16-53`). Coerce converts a value of the wrong type (e.g. a string "42" for a u64) instead of failing the add (CHANGELOG 0.20, #1904).
- Bytes: `BytesOptions` with indexed, fieldnorms, fast, stored (`src/schema/bytes_options.rs:44-100`); bytes fast fields are new in 0.26 (CHANGELOG.md:38).
- Facet and JSON-object fields exist (`src/schema/facet_options.rs`, `src/schema/json_object_options.rs`).
- The schema is 1:1 between an ingested field and an index field: "it is up to the user to duplicate field / concatenate fields before feeding them to tantivy" (ARCHITECTURE.md:118-122). A code-aware second view of a column (see 18) therefore means a second field fed the same text.
- 0.26 adds a fast-field fallback: a `TermQuery` on a field that is FAST but not indexed is answered from the column (CHANGELOG.md:37).

## Fast fields (columnar)
- Fast fields live in the columnar crate `tantivy-columnar-0.7.0`. Numeric columns choose the smallest of three codecs automatically: `Bitpacked`, `Linear`, `BlockwiseLinear` (blocks of 512) (`tantivy-columnar-0.7.0/src/column_values/u64_based/mod.rs:79-92`, chosen by `estimate` at `:172`). There is no user knob.
- String/bytes fast fields keep a per-segment sstable dictionary; its blocks are zstd-compressed when the default `columnar-zstd-compression` feature is on (`tantivy-columnar-0.7.0/Cargo.toml:35`, `tantivy-sstable-0.7.0/src/lib.rs:8`).
- Text `set_fast(Some(tokenizer))` stores the normalized value; the fast-field tokenizer manager is separate (`Index::fast_field_tokenizer`, `src/index/index.rs:408-413`).

## What it means for codectx
- Disk: body is indexed only (no store) in the bench's Rust arm. Remaining stored fields are id, kind, path_s, unit, token_count. All five are also FAST, so `STORED` can be dropped entirely when the page is read from fast columns (the bench's Rust arm's `library-all` mode already does that). Variant: no STORED at all vs current, compare disk bytes (space_usage `store()`), and library query latency.
- Fieldnorms: product mode never reads tantivy's fieldnorms (it uses `token_count`), so `set_fieldnorms(false)` on the five text fields is a pure disk saving there. In library mode it switches BM25 to a constant norm of 1 (`src/query/term_query/term_weight.rs:209-217`), a quality change. Variant: fieldnorms off, compare disk and library-mode pages.
- Record options: `path` and `signature` could drop positions only if phrases never target them; codectx phrases target every column, so keep positions. `kind`, `unit`, `path_s` as `STRING` (Basic) are already minimal.
- Docstore variants: `None`, `Lz4` (default), `Zstd(3)`, `Zstd(9)` x blocksize {16 KiB, 64 KiB}; doc_store_cache_num_blocks {0, 100, 1000}. Compare disk, index time, warm and cold library latency (the 200 fetches).
