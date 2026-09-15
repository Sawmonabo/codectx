# 16. Storage tier 2 — where the remaining bytes are

Date 2026-09-15. This page is the measurement and option inventory behind
[ADR-0003 — Storage tier 2](../adr/ADR-0003-storage-tier2.md), which carries the decisions, the
alternatives and the sources. Read the record first; this page exists so the numbers it quotes can
be checked.

It follows [ADR-0002 — Storage identities](../adr/ADR-0002-storage-identities.md), which cut stored
bytes per indexed symbol by 37–43 % and removed identity width as the amplifier. The wave-I
verification re-measured the §3e gate afterwards and it is still missed: reference corpus 5.47×
eligible source against a 3.5× budget, m32rimm 4.54×, promptfoo 4.30×, redglass 7.11×, r3 4.16×;
2 432–3 600 bytes per indexed symbol against the ~158 B the gate implies. No store was indexed for
this page — the verification lane removed its stores on completion (`rm -rf` on its cache root,
confirmed absent in its Cleanup section), so every byte figure below is quoted from that lane's
per-b-tree attribution and from the storage integration report for the same repository, which
reproduce each other to 0.1 %.

## 1. Where the bytes are

Per-b-tree byte attribution, aggregate mode [S1]. `m32rimm` totals 631 648 256 B (verification lane)
/ 632 266 752 B (integration lane, same corpus, 0.1 % apart — rows the verification lane did not
itemise come from the integration lane's run, marked `†`).

| family | m32rimm B | % of db | r3 B | % of db |
|---|---|---|---|---|
| `search_units` (the `body` column dominates) | 197.5 M | 31.3 | 519.9 M | 27.6 |
| `search_fts_data` (the lexical index) | 53.6 M | 8.5 | 254.7 M | 13.5 |
| `evidence` table | 67.1 M | 10.6 | 202.3 M | 10.7 |
| `idx_evidence_unit` | — | — | 95.4 M | 5.1 |
| `native_keys` table | 34.5 M | 5.5 | — | — |
| `native_keys` automatic index | 38.8 M | 6.1 | — | — |
| `node_facts` † | 25.7 M | 4.1 | — | — |
| `node_ids` + its automatic indexes † | 25.6 M | 4.1 | — | — |
| `relation_ids` + its automatic indexes † | 33.6 M | 5.3 | — | — |
| alias family (table + two indexes) † and relation endpoint index † | 29.9 M | 4.8 | — | — |
| **residual, unattributed** | **125.3 M** | **19.8** | **812.8 M** | **43.1** |

The residual is real and large — neither lane itemised every b-tree — no option below is costed
against it. Three families carry the decision.

- **`search_units` is a second copy of the source.** 73 009 rows hold 197.5 MB on m32rimm, ≈2.7 KB
  a row; every other column is an integer or a short string, so `body` is the rest. The content
  store already holds those bytes uncompressed — which is why the content tier alone is 1.00× on
  both generated corpora (finding SV3) and the ratio has a ~1.0× floor by construction.
- **`body` is write-only in the database.** `search.go:64` declares the hydrated column list as
  "every `search_units` column except body" (§14.2 forbids source bodies in generic results); its
  only readers are the insert (`units.go:709`) and the two paths that feed the external-content
  index a `'delete'` (`delta.go:420`, `gc.go:336`). It exists to service the index, nothing else.
- **`evidence` is six b-trees.** The table, the automatic index over its 32-byte blob primary key,
  and four secondary indexes (`idx_evidence_unit`, `_node`, `_relation`, `_native`), each entry
  carrying the indexed columns plus the table's row key [S2]. On r3 `idx_evidence_unit` is 95.4 MB.

## 2. Options, with what each is worth and what it costs

### 2.1 Full-text options that are already taken or are functional reductions

`search_fts` is **already an external-content table** (`content='search_units'`, `schema.sql:310`)
[S16] and declares **no `prefix=` index**, so "adopt external content" and "remove the prefix
indexes" are worth exactly zero bytes; recorded so the next reader does not re-cost them.

The `detail=` option is the large documented lever and it is refused. Published figures on a
1 636 MiB corpus: 743 / 340 / 134 MiB of index at `full` / `column` / `none` — ≈45 %, ≈21 % and
≈8 % of the indexed text [S3], so on r3's 254.7 MB a 150–210 MB saving. Both are functional
reductions here:

- `detail=column` removes phrase and NEAR queries [S3]. The search tier is built on them:
  `lexical.go:42` treats a multi-token term as a phrase, `:345` counts phrase document frequency
  from term offsets, and quoted input becomes a phrase term at `:206`.
- `detail=none` additionally removes column-filter queries [S3], and the ranking is
  column-weighted (`bm25.go:14`). Losing per-column occurrence would silently change every score.

Neither is a byte-representation change; each deletes an answer the product gives today, so neither
is eligible under the scale posture's own rule that a size fix may not narrow the result.

The trigram tokenizer is the opposite trade: substring and `LIKE`/`GLOB` acceleration [S3] at a
reported ~3× database growth [S4] — a feature decision, refused here on size.

### 2.2 Dropping `body`: a contentless index over the content store (the headline option)

Because `body` is write-only, the column can leave the schema if the index stops reading it back. A
contentless table with `contentless_delete=1` supports `DELETE` and `INSERT OR REPLACE` without a
content table [S5]; it needs engine 3.43.0 [S6] and the embedded engine is **3.53.4** — verified by
running `select sqlite_version()` and then declaring such a table against the pinned driver in a
scratch module; both succeeded.

What it costs and what it needs:

- Snippets and highlights need content [S5]; the range reader in `snapshot/cas.go` serves verified
  byte ranges and the row already stores `file_id`, `start_byte`, `end_byte`, so a body is
  recovered from the content store instead.
- The `'integrity-check'` command run by `schema.go:124` walks the content table into the index.
  Contentless, that cross-check has nothing to walk: internal consistency stays checkable, but the
  content-versus-index agreement check becomes a row-level check against the range reader.
- Indexing throughput **improves**: `delta.go:420` and `gc.go:336` stop reading every body back out
  of the database to feed a delete, and the largest column stops being written at all.
- **Precondition.** The read ceiling in verification finding SV2 (`byte range spans 4292608 bytes,
  over the 1048576-byte read ceiling`) is not user-set and takes the whole answer down. Serving
  bodies from the content store puts it on the search path, so it must first become a user-set,
  unlimited-by-default bound per [ADR-0001](../adr/ADR-0001-scale-posture.md).

Projected saving: `body` ≈ `search_units` less ≈200 B a row — ~182 MB on m32rimm, ~468 MB on r3,
~105 MB on the reference corpus.

### 2.3 `evidence` row compaction

- **Narrow the identity to 16 bytes.** `evidence.id` is a pure function of the row's own fields
  (`model.NewEvidenceID`), so truncating the same digest halves the record field and the automatic
  index key [S2]. At 643 025 rows a 128-bit collision has probability ~10⁻²⁸ and the upsert that
  depends on the uniqueness (`ON CONFLICT(id) DO NOTHING`) is unchanged. ≈21 MB on m32rimm.
- **Drop the stored identity, surrogate integer key.** An `INTEGER PRIMARY KEY` is the row key and
  costs no payload [S7], which removes the 32-byte field, the automatic index and 24 bytes from
  every secondary entry — ~50 MB on m32rimm, ~90 MB on r3. It is conditional: the wire still
  carries the 32-byte value, so it must be recomputable on read, and `NewEvidenceID` hashes the
  canonical string forms of the unit, node and relation identities and the file content hash, all
  of which are now surrogates. That is three to four joins per hydrated row. Costed, not adopted.

Per-file evidence blobs were refused: the four secondary indexes exist because evidence is queried
by unit, node, relation and native key, and a blob answers none of those without a full scan.

### 2.4 `native_keys`: a hash-keyed dictionary, not a rowid-less table

The automatic index over `key` costs more than the table itself on every store (38.8 vs 34.5 MB on
m32rimm, 47.1 vs 41.2 on promptfoo): interning stores each distinct key twice.

Declaring the table `WITHOUT ROWID PRIMARY KEY(key)` does not fix it. Both directions are needed —
the writer resolves key→id, every reader id→key — so the id→key index remains, and a rowid-less
table re-stores its whole primary key inside each secondary index [S8], putting the full key back
into it. Strictly worse than today.

The shape that removes the duplicate is a **hash-keyed dictionary**: `id INTEGER PRIMARY KEY` where
the id is the first 63 bits of the key's digest. One b-tree, the key stored once as payload, and the
writer computes the id without a lookup. Safety is **detection, not improbability**: the insert
compares the stored key against the incoming one and probes `id+1` on disagreement — the read-back
the interner already performs. ≈38 MB on m32rimm.

### 2.5 Content store: block compression under the range reader

There is no page compression in the public-domain engine build; compressed pages are a separately
licensed extension [S9], so application-level block compression is the only way below the 1.0×
floor. The constraint is the block index: `ReadRange` derives a block's offset arithmetically from a
uniform 64 KiB block size, verifies exactly the covering blocks and never rehashes the file. The
change that preserves all of it is to compress each block as an independent frame, add an explicit
per-block compressed-offset array to `model.BlobRecord`, keep `BlockRange` in the plaintext domain
and keep every digest over **plaintext** — integrity, error attribution and the one-block memory
bound unchanged, at 4–8 B a block (≈0.01 %). This is the published seekable framing [S10] and the
block-gzip precedent for indexed random access [S11].

Ratio: level 1 compresses a mixed corpus 2.90× at 422 MB/s, level 3 3.05× at 344 MB/s, level 9
3.53× at 63 MB/s, with decompression ~1.2–1.5 GB/s at every level [S12][S13]. Independent 64 KiB
frames lose cross-frame redundancy, so 2.5× is the conservative planning figure at level 3. On
m32rimm the content tier goes 137.9 → ~55 MB; on r3 288.1 → ~115 MB.

A trained dictionary would add a further documented ~10 % at 64 KiB inputs [S14] and is **not**
recommended: the dictionary becomes a retention invariant — it must outlive every blob that
references it — and the collector has no such lifecycle today.

**Page size and vacuuming.** A new `page_size` takes effect only at the next `VACUUM`, and not in
write-ahead mode [S15], which is the store's permanent mode (`open.go:118`). A larger page changes
fill and fan-out, not payload, and no store carried outstanding write-ahead bytes. Refused: no
measured saving, a real cost.

## 3. Projection, and what the gate should be

Cumulative projection, each step applied to the one before; content tier compressed at 2.5×.

| step | m32rimm db | cas | ratio | B/symbol | r3 db | cas | ratio | B/symbol |
|---|---|---|---|---|---|---|---|---|
| today | 631.6 M | 137.9 M | 4.54× | 3 600 | 1 885.1 M | 288.1 M | 4.16× | 3 296 |
| + drop `body` (§2.2) | 449.6 M | 137.9 M | 3.46× | 2 562 | 1 417.1 M | 288.1 M | 3.26× | 2 478 |
| + evidence 16 B (§2.3) | 428.6 M | 137.9 M | 3.34× | 2 442 | 1 355.1 M | 288.1 M | 3.14× | 2 370 |
| + key dict (§2.4) | 389.8 M | 137.9 M | 3.11× | 2 222 | 1 255.1 M | 288.1 M | 2.95× | 2 195 |
| + cas zstd (§2.5) | 389.8 M | 55.2 M | 2.62× | 2 222 | 1 255.1 M | 115.2 M | 2.62× | 2 195 |

Both repositories pass 3.5× at the first step and land near 2.6×. **Bytes per symbol lands near
2 200 — fourteen times the ~158 B the gate implies — which is the gate's error, not the store's.**
158 is not a derived target: it is a corpus-wide ratio divided by one corpus's symbol density, and
the two are not independent gates, because the ratio moves with source bytes per symbol while the
per-symbol cost does not. Costed from the schema, one indexed symbol on m32rimm carries an identity
row, a fact row, 3.7 evidence rows across six b-trees, 1.6 relation facts with their endpoints, 2.2
alias rows, 0.42 of a search document and its share of the lexical postings — at the per-row costs
the record format gives [S2][S7], ~1 300–1 500 B before any fill loss. The honest post-tier-2 budget
is **≈2 000 B a symbol**; 158 is unreachable for any store that keeps full per-fact provenance. So
retire the single composite ratio and gate the three tiers that scale differently, each against the
quantity it actually scales with:

| tier | budget | projected m32rimm | projected r3 | projected reference |
|---|---|---|---|---|
| content store | ≤ 0.5× eligible source | 0.33× | 0.22× | 0.40× |
| lexical tier (`search_units` + index) | ≤ 0.6× eligible source | 0.41× | 0.59× | 0.45× |
| fact and provenance tier | ≤ 2 000 B a symbol | ~1 900 | ~1 900 | ~1 850 |

A composite miss tells nobody which tier moved; three budgets make a miss name its own cause. The
composite stays published for continuity, as the sum of the three, not as the gate.

## 4. Risks

- **Search first-page latency.** The hydrated page never carried `body` (`search.go:64`), so it is
  unaffected; only a snippet-bearing response pays, one verified block a hit.
- **Indexing throughput.** §2.2 removes the largest write and two read-backs, §2.4 an index write
  per distinct key; §2.5 adds compression at 344 MB/s to a pipeline spending 10–34 minutes on these
  repositories. Net gain expected, to be measured per step.
- **Invariants.** No option introduces a bound. The read ceiling must become user-set and unlimited
  by default *before* §2.2 lands (SV2); peak memory stays a function of the page — one verified
  block a hit, one frame at a time in the writer.
- **Recovery.** The lexical index can no longer be rebuilt or cross-checked from the database
  alone; the content store becomes its rebuild source, and the check is reimplemented first.

## Sources

Every URL cited above appears here once, with what it was used for.

- [S1] *The DBSTAT Virtual Table* — https://www.sqlite.org/dbstat.html — per-b-tree byte attribution
  in aggregate mode; the method behind every figure in §1.
- [S2] *Database File Format* — https://www.sqlite.org/fileformat2.html#record_format — record
  format and serial types; an index entry is the indexed columns followed by the table's row key.
- [S3] *SQLite FTS5 Extension* — https://sqlite.org/fts5.html — the `detail=` option and what each
  setting removes, the 743 / 340 / 134 MiB index measurement over a 1 636 MiB corpus, prefix indexes
  and the trigram tokenizer.
- [S4] *SQLite Forum: trigram indexes* — https://sqlite.org/forum/forumpost/c230760fdf?t=h — a
  trigram index over many small strings growing a database "to 3.7GiB, nearly 3x the original size".
- [S5] *FTS5 — contentless tables* — https://sqlite.org/fts5.html#contentless_tables —
  `contentless_delete=1` supports `DELETE` and `INSERT OR REPLACE`; snippets need content.
- [S6] *SQLite Release 3.43.0 (2023-08-24)* — https://www.sqlite.org/releaselog/3_43_0.html — the
  release that added contentless-delete tables; the version floor for §2.2.
- [S7] *CREATE TABLE — ROWIDs and the INTEGER PRIMARY KEY* —
  https://www.sqlite.org/lang_createtable.html#rowid — an integer primary key *is* the row key.
- [S8] *WITHOUT ROWID Tables* — https://www.sqlite.org/withoutrowid.html — a rowid-less table
  re-stores its whole primary key in every secondary index.
- [S9] *ZIPVFS: Read/Write Compressed Database Files* — https://sqlite.org/com/zipvfs.html — page
  compression is a separately licensed extension, not part of the public-domain amalgamation.
- [S10] *Zstandard Seekable Format* —
  https://github.com/facebook/zstd/blob/dev/contrib/seekable_format/zstd_seekable_compression_format.md
  — independently compressed frames plus a trailing index of compressed and decompressed sizes.
- [S11] *Sequence Alignment/Map Format Specification* (BGZF, §4.1) —
  https://samtools.github.io/hts-specs/SAMv1.pdf — a blocked format with ≤64 KiB blocks and virtual
  offsets; the precedent for indexed random access into compressed data.
- [S12] *lzbench* — https://github.com/inikep/lzbench/blob/master/README.md — Silesia corpus: level
  1 2.89× at 422 MB/s, level 3 3.05× at 344 MB/s, level 9 3.53× at 62.9 MB/s.
- [S13] *Zstandard README benchmark table* — https://github.com/facebook/zstd/blob/dev/README.md —
  "Decompression speed is preserved and remain roughly the same at all settings".
- [S14] *Dictionary compression of small files (issue 468)* —
  https://github.com/facebook/zstd/issues/468 — "Typical gains range from ~10% (at 64KB) to x5
  better (at <1KB)".
- [S15] *PRAGMA page_size* — https://sqlite.org/pragma.html#pragma_page_size — a new page size takes
  effect only at the next `VACUUM`, and not in write-ahead mode.
- [S16] *FTS5 — external content tables* — https://www.sqlite.org/fts5.html#external_content_tables
  — the `content=`/`content_rowid` contract the schema already uses.
- [S17] *Regular Expression Matching with a Trigram Index* —
  https://swtch.com/~rsc/regexp/regexp4.html — a lexical index at ~20 % of the indexed sources (77
  MB over 420 MB); the reference point for the ≤0.6× lexical-tier budget in §3.
