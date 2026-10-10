# 02 — Index settings, field options, field types and fast-field codecs (tantivy 0.26.2)

Sources: the 0.26.2 crate source (`tantivy-0.26.2/src/...`, `tantivy-columnar-0.7.0/src/...`, `tantivy-sstable-0.7.0/src/...`, `tantivy-common-0.11.0/src/...`), its `Cargo.toml.orig` and `CHANGELOG.md`, and https://docs.rs/tantivy/0.26.2/tantivy/index/struct.IndexSettings.html (fetched; it shows the two public fields below).

**codectx context.** Each document has five tokenized columns (name, qualified_name, signature, path, body) plus kind, path, id and token_count. Product rules: complete answers (no top-k cap), deterministic ranking, and retained snapshots with soft-deleted documents.

**What the bench arm's schema does today** (from `../harness/source/src/lib.rs`):
- The five text columns: `WithFreqsAndPositions`, not stored, fieldnorms left at the default (on).
- `id`: `STRING | STORED | FAST`. `kind`, `path_s` and `unit` are the same.
- `node_id`, `name_s`, `qn_s` and `sig_s`: `STRING | FAST`.
- `token_count`: `INDEXED | STORED | FAST`. `start` and `end`: `FAST`.

**Harness vocabulary.** `tindex <docs> <dir> <threads> [default|large|MB] [unit|every:K|end] [--tokenizer unicode61|code]`. A knob with no flag yet is written as an exact setting followed by "needs a harness flag".

---

## 1. IndexSettings: the whole struct

**What it is.** Settings that apply to the whole index. They are serialized into `meta.json` and fixed at creation.

**Exact API.** `pub struct IndexSettings { pub docstore_compression: Compressor, pub docstore_blocksize: usize, #[doc(hidden)] pub docstore_compress_dedicated_thread: bool }` (`tantivy-0.26.2/src/index/index_meta.rs:214-227`). Set with `Index::builder().settings(IndexSettings { .. }).schema(schema).create_in_dir(dir)` (`src/index/index.rs:128,168`).

**Behaviour.**
- **Defaults:** `Compressor::default()`, `docstore_blocksize = 16_384`, `docstore_compress_dedicated_thread = true` (`index_meta.rs:229-241`).
- **That is all of it.** There is no index-sorting setting any more: "remove index sorting [#2434]" is under "Breaking API Changes" for 0.24 (CHANGELOG, 0.24).
- **Status of index sorting: removed.** Doc order is arrival order (see `01-indexing.md` §1, §7). Segment-level ordering by a field is no longer available.

**Applies to codectx?**
- Docstore settings matter only for what is `STORED` (§2).
- The absence of index sorting means a fast-field "ordered by path/id" layout cannot be requested from the library. Deterministic order has to come from the query-side comparator.

**Metric.** Disk and query latency for stored-field fetches.

---

## 2. Docstore compression, levels and block size

**What it is.** The `.store` file is row-oriented storage of `STORED` fields. It is cut into blocks of about `docstore_blocksize` bytes, and each block is compressed on its own.

**Exact API.**
- `Compressor::{None, Lz4, Zstd(ZstdCompressor { compression_level: Option<i32> })}` (`src/store/compressors.rs:10-19,81-84`). `Lz4` exists only with the `lz4-compression` feature; `Zstd` only with `zstd-compression`.
- Serialized names: `"none"`, `"lz4"`, `"zstd"`, `"zstd(compression_level=N)"` (`compressors.rs:21-77,124-130`).
- Reader cache: `IndexReaderBuilder::doc_store_cache_num_blocks(usize)`, default `DOCSTORE_CACHE_CAPACITY = 100` decompressed blocks per segment (`src/reader/mod.rs:57,118`, `src/store/reader.rs:25`).

**Behaviour.**
- **Default compressor.** It is `Lz4` when that feature is on (`compressors.rs:133-143`). The default features are `["mmap", "stopwords", "lz4-compression", "columnar-zstd-compression", "stemmer"]` (`Cargo.toml.orig:116`). **`zstd-compression` is not a default**, so a Zstd docstore needs `tantivy = { version = "=0.26.2", features = ["zstd-compression"] }`.
- **Zstd level.** It defaults to zstd's level 3 when unset (`compressors.rs:82`). Level ≥ 15 logs a warning: "High compression levels (>=15) are slow and will limit indexing speed" (`compressors.rs:108-113`). Brotli and Snappy were removed in 0.21 (CHANGELOG, 0.21).
- **Block size.** A block is flushed when adding the next document would push it over `block_size` (`src/store/writer.rs:65`). Bigger blocks compress better. Every random document fetch decompresses a whole block (`src/index/segment_component.rs:22-24`: "Accessing a document from the store is relatively slow, as it requires to decompress the entire block it belongs to").
- **Merges.** A merge stacks whole compressed blocks without recompressing them only when all of these hold: the segment has no deletes, it has at least 6 blocks, and its compressor matches. Otherwise every live document is re-written (`src/indexer/merger.rs:493-518`).
- **Dedicated thread.** With `docstore_compress_dedicated_thread = true`, a separate thread compresses blocks (`src/store/store_compressor.rs:25-38`).

**Applies to codectx?** Only weakly, and it can be designed away.
- Product mode reads every value it needs (id, path, kind, names, byte range) from fast fields (`../harness/06-extension-points.md`). Only `library` mode fetches the stored `id` for the 200 shown hits.
- Dropping `STORED` from `id`, `kind`, `path_s`, `unit` and `token_count`, and serving `id` from its fast column, shrinks `.store` to almost nothing. That removes the per-document store cost at index time.
- If something must be stored (for example a body for snippets), the compressor and block size trade disk against fetch latency.

**Metric.** Disk (`.store`), index time (compression CPU), and query latency (stored fetches; zero if nothing is stored).

**Benchmark variants** (all need a harness flag):
- `--store none` (no `STORED` flags; `library` mode reads `id` from the fast column) vs the current schema, at `tindex docs.jsonl ix 8 large end`. Report `.store` bytes via `space_usage` (see `04`) and `library` median/p95.
- If any field stays stored: `--docstore lz4` (default) vs `--docstore zstd:3` vs `--docstore zstd:9` vs `--docstore none`, each at `--blocksize 16384` and `--blocksize 65536`. Zstd needs the `zstd-compression` Cargo feature.

---

## 3. Text field options: record options, fieldnorms, stored, fast, coerce

**Exact API.**
- `TextOptions::default().set_indexing_options(TextFieldIndexing).set_stored().set_fast(Option<&str>).set_coerce()` (`src/schema/text_options.rs:129,143,150,157`).
- `TextFieldIndexing::default().set_tokenizer(&str).set_index_option(IndexRecordOption).set_fieldnorms(bool)` (`text_options.rs:224,236,250`).
- `IndexRecordOption::{Basic, WithFreqs, WithFreqsAndPositions}` (`src/schema/index_record_option.rs:15-29`).
- Presets:
  - `STRING` = raw tokenizer, `Basic`, **fieldnorms: true**, not stored, not fast (`text_options.rs:264-273`).
  - `TEXT` = `"default"` tokenizer, `WithFreqsAndPositions`, fieldnorms true (`text_options.rs:276-285`).
  - Flags combine with `|` (`text_options.rs:287-298`).
  - The tokenizer names are `"default"` and `"raw"` (`text_options.rs:166,168`).

**Behaviour.**
- **Record options.**
  - `Basic` stores document ids only (the `IndexRecordOption` default, `index_record_option.rs:18-19`).
  - `WithFreqs` adds term frequency.
  - `WithFreqsAndPositions` adds positions and is "required to run a PhraseQuery" (`index_record_option.rs:24-28`).
  - A reader asking for more than was indexed is downgraded (`index_record_option.rs:51-61`), since 0.20 "Auto downgrade index record option, instead of vint error" (CHANGELOG, 0.20).
- **Fieldnorms.**
  - One byte per document per field, kept for every field that is indexed and has fieldnorms (`src/fieldnorm/writer.rs:22-42`).
  - The encoding is lossy, "using the exact same scheme as Lucene", on a 0–255 log scale (`src/fieldnorm/mod.rs:9-11`). The table is exact for lengths 0–40, then quantized: id 41 = 42, id 50 = 64 (`src/fieldnorm/code.rs:13`).
  - With fieldnorms off, the stock BM25 uses a constant length of 1 (`src/query/term_query/term_weight.rs:209-219`), so there is no length normalization.
- **`STRING` keeps fieldnorms on.** Every `STRING` field in the arm's schema (8 of them) therefore writes 1 byte per document into `.fieldnorm`. A raw-tokenized field always has length 1, so the byte carries no information. The default buys "`STRING` fields are scorable like any other field", at that cost.
- **Text fast fields.** `set_fast(None)` stores the raw text as a dictionary-encoded column. `set_fast(Some(name))` first normalizes with a tokenizer from the separate fast-field tokenizer manager (`text_options.rs:113-139`, `Index::fast_field_tokenizer`, `src/index/index.rs:413`). The column holds term ordinals, and strings come back through the dictionary (`text_options.rs:125-127`).
- **Coerce** matters only when building a document from JSON (`FieldType::value_from_json`, `src/schema/field_type.rs:360-365`). With coerce, a JSON number put into a text field becomes a string instead of an error. Added in 0.20 (CHANGELOG, 0.20). Typed `TantivyDocument::add_text` and `add_u64` calls never coerce.

**Applies to codectx?** Yes.
- **The five text columns need `WithFreqsAndPositions`.** Product scoring needs tf, and its phrase counting needs positions (`../harness/06-extension-points.md`).
- **The five text columns do not need fieldnorms in product mode**, which takes the document length from `token_count`. They do need them for `library` mode, which uses stock BM25.
- **Every `STRING` field can turn fieldnorms off.** Build it as `TextOptions::default().set_indexing_options(TextFieldIndexing::default().set_tokenizer("raw").set_index_option(IndexRecordOption::Basic).set_fieldnorms(false))`.

**Metric.** Disk (`.pos` dominates for code bodies; `.fieldnorm` is 1 byte × documents × fields), index time (positions are the most expensive postings to write), and quality (lossy fieldnorms are one more reason stock BM25 ≠ codectx BM25F).

**Benchmark variants** (all need a harness flag):
- `--fieldnorms text-only` (off for all `STRING` fields), and `--fieldnorms none` (also off for the five text fields; product mode only).
- `--record body=freqs` (body as `WithFreqs`, no phrases on body). This is a quality probe: run `teval` on the phrase queries to see what is lost.
- Run each at `tindex docs.jsonl ix 8 large end`. Report per-component bytes (`04`), the `-add` time, and `tquery … product`.

---

## 4. Numeric fields: u64, i64, f64, bool

**Exact API.**
- `SchemaBuilder::add_u64_field / add_i64_field / add_f64_field / add_bool_field(name, impl Into<NumericOptions>)` (`src/schema/schema.rs:50,66,82,98`).
- `NumericOptions::default().set_indexed().set_fieldnorm().set_fast().set_stored().set_coerce()` (`src/schema/numeric_options.rs:87,97,109,119,128`).

**Behaviour.**
- **The `INDEXED` flag turns fieldnorms on** for a numeric field: `From<IndexedFlag>` sets `indexed: true, fieldnorms: true` (`numeric_options.rs:176-186`). Fieldnorms only take effect when the field is indexed (`numeric_options.rs:14,69-71`).
- **Indexed numbers** go into the term dictionary and postings, which lets `TermQuery` and `RangeQuery` run on the inverted index.
- **Fast numbers** become columnar values (§9).
- **A `TermQuery` on a field that is fast but not indexed** falls back to an exact `RangeQuery` scan of the fast column when scoring is disabled. The source notes: "This is considerable slower since it requires to scan the entire fast field" (`src/query/term_query/term_query.rs:126-142`. `Type::Str` is one of the eligible types, `src/query/range_query/mod.rs:13-25`, so a `delete_term` on a `FAST`-only string field is a full column scan, not an error; "Add fast field fallback for `TermQuery` when the field is not indexed", CHANGELOG, 0.26).

**Applies to codectx?** Yes. The current schema pays for options it does not read: `INDEXED` and `STORED` on a value that is only read as a column. That is a cost/benefit choice, not an error. Indexing keeps range and term queries on the inverted index available.
- `token_count` is read only as a fast column (document length). `INDEXED | STORED | FAST` adds postings, a term dictionary and fieldnorms, and also stores it.
- `FAST` alone is enough for `token_count`, and also for `start` and `end`, which are already `FAST` only.

**Metric.** Disk and index time.

**Benchmark variant.** `--lean-numeric` (`token_count` as `FAST` only) at `tindex docs.jsonl ix 8 large end`. Needs a harness flag. Product results must stay identical: check with `tcompare`.

---

## 5. Date, bytes, IP and facet fields

**Exact API.**
- `add_date_field(name, DateOptions)`: `DateOptions::{set_indexed, set_fieldnorm, set_fast, set_stored, set_precision(DateTimePrecision)}` (`src/schema/date_time_options.rs:56,68,78,87,97`). `DateTimePrecision::{Seconds (default), Milliseconds, Microseconds, Nanoseconds}` (`tantivy-common-0.11.0/src/datetime.rs:16-26`). Indexed terms use `DATE_TIME_PRECISION_INDEXED = Seconds` (`date_time_options.rs:9`).
- `add_bytes_field(name, BytesOptions)`: `set_indexed`, `set_fieldnorms`, `set_fast`, `set_stored` (`src/schema/bytes_options.rs:71,81,90,100`). Fast support for bytes was added in 0.26 ("Add fast field support for `Bytes` values", CHANGELOG, 0.26).
- `add_ip_addr_field(name, IpAddrOptions)`: `set_indexed`, `set_fieldnorms`, `set_fast`, `set_stored` (`src/schema/ip_options.rs:62,69,82,91`). Values are IPv6 `u128`s.
- `add_facet_field(name, FacetOptions)` (`src/schema/schema.rs:159`). `FacetOptions` has only `set_stored`: "a Facet is always indexed and stored as a fastfield" (`src/schema/facet_options.rs:9-31`).

**Facet behaviour.** The `FacetTokenizer` "emits a token for all of its parent": `/america/north_america/canada` is indexed as `/america/north_america/canada`, `/america/north_america` and `/america` (`src/tokenizer/facet_tokenizer.rs:4-11`, used at `src/indexer/segment_writer.rs:177-191`). Build values with `Facet::from_text` or `Facet::from_path` (`src/schema/facet.rs:86,131`).

**Applies to codectx?**
- **Facet: yes, for the path filter.** codectx's path rule is "whole folders: `P` itself or under `P/`" (`../harness/00-library-arm.md`). Indexing each document's path as a facet `/a/b/c.go` makes `TermQuery(Term::from_facet(path_facet, &Facet::from("/a/b")))` match exactly the documents at `/a/b` or under `/a/b/`, never `/a/bc`. That is one posting-list lookup instead of the product's ordinal-range filter.
  - Cost: one term per path depth per document.
  - Caveat: `/` is the separator, so path segments that contain characters the facet parser escapes need checking.
- Date, bytes and IP: no codectx column needs them.

**Metric.** Query latency for path-filtered queries, and disk (facet terms).

**Benchmark variant.** `--path-facet` adds a facet field built from `path`, and `tquery … --path-prefix pkg/api` uses the facet `TermQuery` as the filter. Needs a harness flag. Hits must equal the current ordinal filter on every filtered query (`tcompare`); report median/p95.

---

## 6. JSON fields

**Exact API.** `add_json_field(name, JsonObjectOptions)`, with `JsonObjectOptions::{set_stored, set_fast(Option<&str>), set_indexing_options(TextFieldIndexing), set_expand_dots_enabled}` (`src/schema/json_object_options.rs:101,118,139,153`).

**Behaviour.**
- One field holds arbitrary nested keys. Each path's values are indexed with the field's tokenizer, or kept fast.
- With `expand_dots_enabled`, `{"a.b": 1}` is treated like `{"a": {"b": 1}}` (`json_object_options.rs:37-44,84-102`).
- Since 0.24, any JSON value is accepted at the root, not only objects (CHANGELOG, 0.24).

**Applies to codectx?** No. The schema is fixed and typed, and JSON paths would put all five columns into one field with one set of statistics, losing the per-column tf that BM25F needs.

---

## 7. Fast fields and the columnar format (codecs)

**What it is.** The `.fast` file is one columnar store per segment. Each fast field becomes one or more typed columns: a column index plus column values (`tantivy-columnar-0.7.0/src/column/serialize.rs:28-41`).

**Codecs: chosen automatically, with no user knob.**
- **u64-mapped columns** (u64, i64, f64, bool, date). The serializer gathers statistics, estimates each candidate codec's size, and **keeps the smallest** (`tantivy-columnar-0.7.0/src/column_values/u64_based/mod.rs:147-186`). The candidates are `[Bitpacked, BlockwiseLinear]` (`column/serialize.rs:34-37`). Plain `Linear` is defined but not offered here (`u64_based/mod.rs:79-96`).
  - `Bitpacked`: bits = the amplitude `max - min`.
  - `BlockwiseLinear`: "Same as Linear, but encodes in blocks of 512 elements". It fits a line and bitpacks the deviations.
  - The statistics also track a GCD of increments, so values in fixed steps shrink further (`u64_based/stats_collector.rs:8-39`).
- **u128 columns** (IP) use a compact-space codec (`column_values/u128_based/`).
- **String and bytes columns** are dictionary-encoded: an sstable dictionary of sorted terms plus a u64 column of term ordinals (`columnar/writer/mod.rs:392-400`, `dictionary.rs:16-30`). Ordinals order exactly as the bytes do, which product mode relies on.
- **Sstable blocks** hold about 4 000 bytes (`tantivy-sstable-0.7.0/src/delta.rs:13`). A block over 2 048 bytes is zstd-compressed at level 3 and kept only if that is smaller (`delta.rs:56-77`). This uses the default `columnar-zstd-compression` feature (`Cargo.toml.orig:125`).
- **Column index by cardinality.** Full (every document has one value), Optional, or Multivalued. Optional uses dense or sparse 65 536-document blocks: dense above 5 120 values per block, sparse otherwise (`column_index/optional_index/mod.rs:19-44`). Multivalued columns got an optional index for mostly-empty cases in 0.24 (CHANGELOG, 0.24).

**Applies to codectx?** Yes. Product mode is built on fast columns (`token_count`, `start`, `end` and seven string columns, `../harness/06-extension-points.md`).
- `token_count`, `start` and `end` are full u64 columns. `start`/`end` grow with document order inside a file, so `BlockwiseLinear` may win: it is measured, not assumed.
- **The cost is the double representation; the benefit is that term queries and deletes on these fields use postings.** `name_s`, `qn_s`, `sig_s` and `node_id` are `STRING | FAST`, so they are also indexed: term dictionary, postings and fieldnorms. If they are only read as columns, `FAST` alone (`TextOptions::default().set_fast(None)`) drops the inverted side.
- **`id` must stay indexed** (or deletes on it become fast-column scans, §4) only if deletes or lookups go by `id`. A delta rebuild that deletes by `unit` does not need `id` indexed.

**Metric.** Disk (`.fast` against `.term` + `.idx` + `.fieldnorm`), index time, and query latency (column reads are the product mode's inner loop).

**Benchmark variants** (needs a harness flag):
- `--lean-strings` (`name_s`, `qn_s`, `sig_s` and `node_id` as `FAST` only; `unit`, `kind` and `path_s` stay `STRING | FAST` for deletes and filters) against the current schema, at `tindex docs.jsonl ix 8 large end`. Product output must be identical (`tcompare` overlap 200/200). Report the per-field `.fast`, `.term` and `.idx` bytes from `space_usage` (see `04`).
- There is no codec override to benchmark. Record which codec each u64 column chose: the first byte of its values block is the codec code (`u64_based/mod.rs:179`), or read the column sizes from `space_usage`.

---

## 8. Combined "lean schema" variant (recommended single run)

One run that combines §2–§7, so the measurement can cost the whole change at once. Needs a harness flag `--schema lean`.
- **Text columns:** `TextFieldIndexing::default().set_tokenizer(<unicode61|code>).set_index_option(WithFreqsAndPositions).set_fieldnorms(<true for library-mode comparability; false for product-only>)`, not stored.
- **`id`, `unit`, `kind`, `path_s`:** raw, `Basic`, fieldnorms off, `FAST`, not stored.
- **`node_id`, `name_s`, `qn_s`, `sig_s`:** `FAST` only.
- **`token_count`, `start`, `end`:** `FAST` only.
- **Settings:** the default `IndexSettings` (nothing stored, so the compressor is moot).

Run `tindex docs.jsonl ix-lean 8 large end` against `tindex docs.jsonl ix 8 large end`. Compare:
- disk per component and in total;
- `-add` and `-final` time and peak RSS;
- `tquery … product` and `… library-all` median/p95;
- `tcompare` (product: identical);
- `teval` (library: delta from losing fieldnorms, if they were turned off).

---

## 9. Web sources consulted (with credibility)

**Primary sources: maintainers and official.**
- docs.rs, `IndexSettings` at 0.26.2: https://docs.rs/tantivy/0.26.2/tantivy/index/struct.IndexSettings.html. It shows only `docstore_compression` and `docstore_blocksize` as public (the dedicated-thread flag is `#[doc(hidden)]`), which matches §1.
- Quickwit, "Tantivy 0.19 is out", January 18 2023: https://quickwit.io/blog/tantivy-0.19. "the doc store now uses a separate thread to compress the block store", with about 50% faster indexing in their experiments, and the doc store "has now configurable compression levels and block sizes". Maintainer post; no hardware stated.
- Quickwit, "Tantivy 0.22", April 16 2024: https://quickwit.io/blog/tantivy-0.22. Less memory while indexing (docid deltas; 760 → 590 MB on 1.1 GB of HDFS logs) and faster sstable loading. Maintainer post.
- The 0.24 removal of index sorting (#2434) and the 0.26 additions (bytes fast fields #2830, the `TermQuery` fast-field fallback #2693, `space_usage` for columnar data #2761) come from the crate's own CHANGELOG, a primary source.

**Secondary source: another project's measured change (code search, comparable domain).**
- open-kioku PR #573, merged September 27 2026: https://github.com/shivyadavus/open-kioku/pull/573.
  - It stopped indexing a whole serialized symbol record, and made it stored-only.
  - It indexed a new text field holding only name, qualified name, signature and declaring path, with a code tokenizer.
  - Reason: metadata words "matched nearly every chunk that had a symbol", and changing stored metadata alone "shifted BM25 document lengths".
  - Measured: holdout R@5 0.722 → 0.778 with MRR unchanged, but lexical dev R@5 0.860 → 0.833.
  - This is another project's harness, but it is direct evidence that *which* fields are indexed, against stored only, moves quality in code search. It supports measuring the lean-schema variant (§8) for quality as well as disk.
