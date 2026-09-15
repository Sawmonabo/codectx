# ADR-0003 — Storage tier 2: the lexical and provenance tiers, and the gate

- **Status:** §2.1 Accepted and implemented; §2.2–§2.5 Proposed
- **Date:** 2026-09-15
- **Follows:** [`ADR-0002 — Storage identities`](ADR-0002-storage-identities.md), which removed
  identity width as the amplifier and left the lexical and provenance tiers as the remaining cost
- **Refines:** [`ADR-0001 — Scale posture`](ADR-0001-scale-posture.md) — every decision here is
  bound by its rule that a size fix may not narrow an answer
- **Measurements and option inventory:**
  [`docs/research/16-storage-tier2.md`](../research/16-storage-tier2.md)
- **Budget row:** [`docs/performance.md`](../performance.md) row 13

---

## 1. Context

ADR-0002 cut stored bytes per indexed symbol by 37–43 % on every corpus and was verified
independently afterwards. The gate is still missed. Stored bytes over eligible source bytes:
reference corpus **5.47×** against a 3.5× budget, m32rimm **4.54×**, promptfoo **4.30×**, redglass
**7.11×**, r3 **4.16×**; bytes per indexed symbol **2 432–3 600** against the ~158 B that gate
implies.

Per-b-tree attribution says where the bytes now are, and it is no longer identity width. On every
store the two largest families are the same: `search_units` plus the lexical index is **31–44 %** of
the database, and evidence with its indexes another ~20 %. `search_units` is 73 009 rows and
197.5 MB on m32rimm — ≈2.7 KB a row, essentially all of it the `body` column, which holds a second
uncompressed copy of source the content store already holds. That second copy is why the content
tier measures exactly 1.00× on both generated corpora, and therefore why the whole ratio has a
floor of ~1.0× by construction: a 3.5× gate is really a ~2.5× budget for the database.

Two facts about the schema make the shape of the fix specific rather than speculative. `body` is
**write-only in the database** — the hydrated column list is every `search_units` column except it,
because §14.2 forbids source bodies in generic results, and its only readers are the insert and the
two paths that feed the external-content index a delete. And `evidence` is **six b-trees**: the
table, an automatic index over a 32-byte blob primary key, and four secondary indexes each carrying
the indexed columns followed by the table's row key [S1].

---

## 2. Decision

### 2.1 The lexical tier stops storing a second copy of the source

`search_units.body` is removed. The full-text index becomes contentless with
`contentless_delete = 1`, which supports `DELETE` and `INSERT OR REPLACE` without a content table
[S2]; it requires engine 3.43.0 [S3] and the embedded engine is 3.53.4 — verified by declaring such
a table against the pinned driver. Snippets and highlights, which need content [S2], are served
from the content store through the existing verified range reader, using the `file_id`,
`start_byte` and `end_byte` the row already carries.

**Alternatives steel-manned.** *Adopt external content* and *remove the prefix indexes* are the
obvious first moves and both are already true: the index is declared `content='search_units'` today
[S4] and declares no `prefix=` option, so each is worth exactly zero bytes. *Lower `detail=`* is the
large documented lever — a published corpus measurement puts the index at ≈45 % of the indexed text
at `full`, ≈21 % at `column` and ≈8 % at `none` [S5], which on r3 is a 150–210 MB saving. It is
refused: `detail=column` removes phrase and NEAR queries and `detail=none` additionally removes
column-filter queries [S5], and the search tier is built on both — multi-token terms are phrases,
phrase document frequency is counted from term offsets, and ranking is column-weighted. That is not
a change of representation, it deletes answers the product gives today. *Switch to the trigram
tokenizer* buys substring matching [S5] at a reported ~3× growth [S6]; it is a feature question, and
on size it goes the wrong way.

**Why this wins.** It is the only option in the family that removes bytes without removing an
answer: the bytes deleted are a duplicate of bytes the store already keeps, verified, elsewhere.

**Trade-off accepted.** The index can no longer be rebuilt or cross-checked from the database
alone. The integrity command that today walks the content table into the index has nothing to walk,
so the content-versus-index agreement check must be reimplemented against the range reader
**before** this lands, and the rebuild source becomes the content store. In exchange, indexing
gets faster: the largest column stops being written, and the delta and collection paths stop
reading every body back out to feed a delete.

### 2.2 Evidence rows carry a 16-byte identity

`evidence.id` is narrowed from a 32-byte digest to the first 16 bytes of the same digest, halving
both the record field and its automatic index key [S1].

**Alternatives steel-manned.** *Drop the stored identity for an integer row key* is worth roughly
twice as much — an integer primary key is the row key and costs no payload [S7], removing the field,
the automatic index and 24 bytes from every secondary entry, ~50 MB on m32rimm. It is not adopted
**yet** because the 32-byte value reaches the wire and would have to be recomputed on read, and the
derivation hashes the canonical string forms of unit, node, relation and file-content identities
that the schema now stores as surrogates: three to four joins per hydrated row, an unmeasured
latency cost on the hottest read path. *Pack evidence into a per-file blob* is refused outright: the
four secondary indexes exist because evidence is queried by unit, node, relation and native key, and
a blob answers none of those without a scan.

**Why this wins.** At 643 025 rows a 128-bit digest's collision probability is ~10⁻²⁸ and the upsert
that depends on the uniqueness is unchanged, so the saving is free of both risk and joins.

**Trade-off accepted.** The wire identity narrows to 16 bytes, which is a visible format change, and
it forecloses nothing: the integer-key variant remains available once the recomputation cost is
measured.

### 2.3 The key dictionary becomes hash-keyed

`native_keys` keeps `id INTEGER PRIMARY KEY`, but the id becomes the first 63 bits of the key's
digest and the unique index over `key` is dropped. The writer computes the id without a lookup; the
key is stored once, as payload.

**Alternative steel-manned.** *Declare the dictionary `WITHOUT ROWID` keyed by the key* is the
natural reading of "the automatic index costs more than the table" (38.8 MB against 34.5 MB on
m32rimm). It is refused on the format: both directions are needed — the writer resolves key to id,
every reader resolves id to key — so an id-keyed index survives, and a rowid-less table re-stores
its whole primary key inside every secondary index [S8], which would put the full key back into it.
Strictly worse than today.

**Why this wins.** It is the only shape that leaves one b-tree and one copy of each key. Safety is
**detection, not improbability**: the insert compares the stored key against the incoming key and
probes the next id on disagreement, reusing the read-back the interner already performs.

**Trade-off accepted.** Ids stop being dense and sequential, and a collision costs an extra probe.

### 2.4 The content store compresses its blocks, under an explicit block index

Each 64 KiB block becomes an independently compressed frame. `model.BlobRecord` gains an explicit
per-block compressed-offset array; block-range arithmetic stays in the plaintext domain and every
digest stays over **plaintext**, so integrity, per-block error attribution and the one-block memory
bound are unchanged. The index costs 4–8 B a block — ≈0.01 % of the blob. Independent frames plus a
frame index is the published seekable framing [S9] and the block-gzip precedent for indexed random
access into compressed data [S10].

**Alternatives steel-manned.** *Compress the database instead* is not available: there is no page
compression in the public-domain engine build, only a separately licensed extension [S11]. *Raise
the page size and vacuum* changes fill and fan-out, not payload, and takes effect only at a vacuum
and not in write-ahead mode [S12], which is the store's permanent mode — refused, no measured
saving. *Train a dictionary* adds a documented ~10 % at 64 KiB inputs [S13] and is refused because
the dictionary becomes a retention invariant the collector cannot express: it must outlive every
blob that references it.

**Why this wins.** It is the only change that moves the content tier below its 1.0× floor, and at
level 3 a mixed corpus compresses 3.05× at 344 MB/s with decompression ~1.2–1.5 GB/s at every level
[S14][S15]; 2.5× is the conservative planning figure for 64 KiB frames.

**Trade-off accepted.** Every range read decompresses one frame; the store's on-disk objects stop
being byte-identical to the source file.

### 2.5 The gate becomes three budgets, one per tier

Row 13's single ratio is retired as the gate and replaced by three, each measured against the
quantity that tier actually scales with: **content store ≤ 0.5× eligible source**, **lexical tier
≤ 0.6× eligible source**, **fact and provenance tier ≤ 2 000 B per indexed symbol**. The composite
figure stays published as their sum, for continuity, not as the gate.

**Alternative steel-manned.** *Keep 3.5× and make the store meet it.* On the projection it would in
fact be met — both real repositories pass 3.5× at the first step and land near 2.6× — so this is not
a plea for a weaker number. It is refused because the pair of gates is incoherent: ~158 B a symbol
is not a derived target but a corpus-wide ratio divided by one corpus's symbol density, and the
ratio moves with source bytes per symbol while the per-symbol cost does not. Costed from the
schema's own row sizes [S1][S7], one symbol carries an identity row, a fact row, 3.7 evidence rows
across six b-trees, 1.6 relation facts, 2.2 alias rows and a share of a search document —
~1 300–1 500 B before fill loss. 158 B is unreachable for any store that also keeps a lexical index
and full per-fact provenance, and a gate that cannot be met by a correct implementation is a gate
that will be met by degrading the answer.

**Why this wins.** A composite miss names no cause; three budgets each name their own. The external
reference point for the lexical budget is a published lexical index at ~20 % of the indexed sources
[S16]; the content budget is what block compression delivers; the per-symbol budget is derived above
and is ~14× the number it replaces because that number was never derived at all.

**Trade-off accepted.** Row 13 stops being one comparable number across the project's history.

### 2.6 Sequence

1. **Precondition.** The content-store read ceiling that verification finding SV2 records — `byte
   range spans 4292608 bytes, over the 1048576-byte read ceiling`, a bound no user set, which takes
   the whole answer down instead of truncating and flagging — becomes a user-set bound that is
   unlimited by default, and the integrity check is reimplemented. §2.1 must not land first:
   it puts that ceiling on the search path.
2. §2.1 lexical tier. 3. §2.2 evidence identity. 4. §2.3 key dictionary. 5. §2.4 content store.

Projected, each step applied to the one before (content tier compressed at 2.5×):

| step | m32rimm ratio | m32rimm B/symbol | r3 ratio | r3 B/symbol |
|---|---|---|---|---|
| today | 4.54× | 3 600 | 4.16× | 3 296 |
| + §2.1 | 3.46× | 2 562 | 3.26× | 2 478 |
| + §2.2 | 3.34× | 2 442 | 3.14× | 2 370 |
| + §2.3 | 3.11× | 2 222 | 2.95× | 2 195 |
| + §2.4 | 2.62× | 2 222 | 2.62× | 2 195 |

---

## 3. Consequences

- **Query latency.** The hydrated search page never carried a body, so first-page time is
  unaffected; only a snippet-bearing response pays, one verified block a hit. Every step must be
  measured against first-page time before the next lands.
- **Indexing throughput.** §2.1 removes the largest write and two read-backs and §2.3 removes an
  index write per distinct key; §2.4 adds compression at 344 MB/s to a pipeline that spends 10–34
  minutes on these repositories. A net gain is expected and must be shown, not assumed.
- **Invariants.** No step introduces a bound. Peak resident memory stays a function of the page:
  one verified block a hit, one frame at a time in the writer.
- **Recovery.** The lexical index's rebuild source moves from the database to the content store;
  this is a durability statement about the content store and it must be stated in
  [`docs/storage.md`](../storage.md) when §2.1 lands.

---

## 4. Sources

Every URL cited above appears here once, with what it was used for. Body citations are by `[Sn]`.

[S1] *Database File Format* — https://www.sqlite.org/fileformat2.html#record_format — record format
and serial types; an index entry is the indexed columns followed by the table's row key. The basis
of the evidence six-b-tree accounting and of the per-symbol derivation in §2.5.

[S2] *SQLite FTS5 Extension — contentless tables* —
https://sqlite.org/fts5.html#contentless_tables — `contentless_delete=1` supports `DELETE` and
`INSERT OR REPLACE`; snippets and highlights require content. The basis of §2.1.

[S3] *SQLite Release 3.43.0 (2023-08-24)* — https://www.sqlite.org/releaselog/3_43_0.html — the
release that added contentless-delete tables; the version floor checked against the embedded engine.

[S4] *SQLite FTS5 Extension — external content tables* —
https://www.sqlite.org/fts5.html#external_content_tables — the `content=`/`content_rowid` contract
the schema already uses; why "adopt external content" is worth nothing here.

[S5] *SQLite FTS5 Extension* — https://sqlite.org/fts5.html — the `detail=` option and what each
setting removes, the 743 / 340 / 134 MiB index measurement over a 1 636 MiB corpus, prefix indexes,
and the trigram tokenizer; the refusals in §2.1.

[S6] *SQLite Forum: trigram indexes* — https://sqlite.org/forum/forumpost/c230760fdf?t=h — a trigram
index over many small strings growing a database "to 3.7GiB, nearly 3x the original size".

[S7] *CREATE TABLE — ROWIDs and the INTEGER PRIMARY KEY* —
https://www.sqlite.org/lang_createtable.html#rowid — a single-column integer primary key *is* the
row key: zero payload bytes; the value of the deferred variant in §2.2.

[S8] *WITHOUT ROWID Tables* — https://www.sqlite.org/withoutrowid.html — a rowid-less table
re-stores its whole primary key in every secondary index; why §2.3's alternative is worse.

[S9] *Zstandard Seekable Format* —
https://github.com/facebook/zstd/blob/dev/contrib/seekable_format/zstd_seekable_compression_format.md
— independently compressed frames plus a trailing index of compressed and decompressed sizes.

[S10] *Sequence Alignment/Map Format Specification* (BGZF, §4.1) —
https://samtools.github.io/hts-specs/SAMv1.pdf — a blocked compressed format with ≤64 KiB blocks and
virtual offsets; the precedent for indexed random access into compressed data.

[S11] *ZIPVFS: Read/Write Compressed Database Files* — https://sqlite.org/com/zipvfs.html — page
compression is a separately licensed extension, not part of the public-domain amalgamation.

[S12] *PRAGMA page_size* — https://sqlite.org/pragma.html#pragma_page_size — a new page size takes
effect only at the next vacuum, and not in write-ahead mode.

[S13] *Dictionary compression of small files (issue 468)* —
https://github.com/facebook/zstd/issues/468 — "Typical gains range from ~10% (at 64KB) to x5 better
(at <1KB)"; the size of the refused dictionary option.

[S14] *lzbench, an in-memory benchmark of open-source compressors* —
https://github.com/inikep/lzbench/blob/master/README.md — Silesia corpus: level 1 2.89× at
422 MB/s, level 3 3.05× at 344 MB/s, level 9 3.53× at 62.9 MB/s.

[S15] *Zstandard README benchmark table* — https://github.com/facebook/zstd/blob/dev/README.md —
"Decompression speed is preserved and remain roughly the same at all settings".

[S16] *Regular Expression Matching with a Trigram Index* —
https://swtch.com/~rsc/regexp/regexp4.html — a lexical index at ~20 % of the indexed sources (77 MB
over 420 MB); the external reference point for the lexical-tier budget.

---

*Every figure in this record is quoted from the wave-I verification run's per-b-tree attribution or
derived in [`docs/research/16-storage-tier2.md`](../research/16-storage-tier2.md), where the
arithmetic is shown. §2.1 has been implemented; §2.2–§2.5 have not.*

**Implementation note on §2.1.** The record says `body`'s only database readers are the insert and
the two paths that feed the index a delete. There is a third: the delta carry-over copies a previous
unit's lexical documents and re-indexes them at their new rowids, and it read `body` back to do so.
Contentless, there is nothing to read back, so the carry-over now resolves each carried document's
text from the content store through the verified range reader — over the byte range the copied row
carries and the content hash this unit declares for that file, which `checkCarriedInputs` has
already proved — in bounded pages, so its working set is one page of documents. A store opened
without a range reader refuses that carry-over with a typed error rather than indexing a document
without its body. This is the same "rebuild source becomes the content store" consequence the
trade-off names, reaching one more path than the record anticipated.
