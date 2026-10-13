# ADR-0003 — Storage tier 2: the lexical and provenance tiers, and the gate

- **Status:** Partially accepted — §2.1, §2.2, §2.3 and §2.4 implemented (§2.2 narrows the public evidence identity to 16 bytes and moves the pinned capsule digest; §2.4 keeps content-store objects as uncompressed plaintext, chosen by measurement for speed and memory); §2.5 is a measurement gate, not code. Its lexical tier — §2.1, its place in the §2.6 sequence and its §3 consequences for indexing throughput and recovery — is superseded by [ADR-0013](ADR-0013-search-index.md), 2026-10-08
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
reference corpus **5.47×** against a 3.5× budget, a large Python repository **4.54×**, promptfoo **4.30×**, the second corpus (a configured,
Python-majority polyglot monorepo) **7.11×**, the reference repository (an unconfigured, JavaScript-majority
repository with its dependencies committed) **4.16×**; bytes per indexed symbol **2 432–3 600** against the ~158 B that gate
implies.

Per-b-tree attribution says where the bytes now are, and it is no longer identity width. On every
store the two largest families are the same: `search_units` plus the lexical index is **31–44 %** of
the database, and evidence with its indexes another ~20 %. `search_units` is 73 009 rows and
197.5 MB on the large Python repository — ≈2.7 KB a row, essentially all of it the `body` column, which holds a second
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
at `full`, ≈21 % at `column` and ≈8 % at `none` [S5], which on the reference repository is a 150–210 MB saving. It is
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

`evidence.id` is the first 16 bytes of the occurrence's SHA-256 derivation instead of all 32, and
the public `EvidenceID` is the same value, 32 hex characters. The record field, the primary key's
automatic index key and `idx_evidence_unit(unit_id, id)` each halve [S1]; the other three evidence
indexes carry the rowid and do not change.

**Measured.** 200 000 evidence rows of the store's own shape (a third node subjects, two thirds
relation subjects, all six b-trees, vacuumed): 47 915 008 B at 32 bytes, 38 002 688 B at 16 —
**49.6 B a row, 20.7 % of the evidence family**: the table 22 167 552 → 18 669 568 B, the automatic
index 8 212 480 → 5 005 312 B, `idx_evidence_unit` 8 810 496 → 5 603 328 B. Nothing on the read path
changes but the bytes compared: every evidence read already selects `e.id` and hex-encodes it.

**What it does to the product surfaces.** The wire `evidence_id` (reference occurrences, relation
occurrences, fact references, row marks, path evidence) narrows from 64 to 32 hex characters, and
every validator of an evidence identity takes that width and refuses the full one. The pinned
capsule digest moves, because a fact reference hashes its evidence identities: the model test's
full fixture pins `752b510d…f41a` where it pinned `2b4713c7…8ad3`. Both are format changes with
no consumer to migrate.

**Exactness.** The identity is still a pure function of the occurrence, so the upsert that makes a
republished occurrence idempotent, the keyset order of evidence pages and the clip that keeps a
fact's lowest ids are unchanged in kind, and a prefix keeps the digest's order, so each is the
order the full digest would give. Two distinct occurrences in one store collide with probability
about n²/2¹²⁹: ≈10⁻²⁵ at ten million rows. A truncated SHA-256 keeps collision resistance of half
its length, 64 bits against a deliberate search [S20]; a collision would drop one occurrence of a
fact that has others, and producing one means a 2⁶⁴-work search over occurrence fields a
repository author controls only in part.

**Alternatives steel-manned.** *Drop the stored identity for an integer row key* removes the field
and the automatic index outright [S7] and is refused on exactness, not deferred. The upsert needs a
unique key, and the occurrence's natural key — unit, subject, precision, native key, file, range,
detail, dispatch, set size — is far wider than 16 bytes as an index. The clip and the keyset pages
order by the id, so a row key would make the retained set and the page boundaries a function of
insertion order, which a delta changes; and a rowid on the wire is what the identity model forbids
(Section 9.1). Recomputing the digest on read to avoid that costs three to four joins per row on
the hottest read path and still leaves the ordering problem. *Key the table by a 63-bit digest
prefix with collision detection*, as §2.3 does for native keys, is smaller still but a detected
collision probes to the next free id, so an occurrence's identity would depend on which colliding
row was written first — identity stops being a function of content. *Keep 32 bytes* gives up
49.6 B a row for a collision margin that is already beyond any row count a store reaches.

**Why this wins.** It is the smallest form that keeps the identity a pure function of the
occurrence and every ordering over it exact.

**Trade-off accepted.** A visible narrowing of the wire identity and a moved capsule digest.

### 2.3 The key dictionary becomes hash-keyed

`native_keys` keeps `id INTEGER PRIMARY KEY`, but the id becomes the first 63 bits of the key's
digest and the unique index over `key` is dropped. The writer computes the id without a lookup; the
key is stored once, as payload.

**Alternative steel-manned.** *Declare the dictionary `WITHOUT ROWID` keyed by the key* is the
natural reading of "the automatic index costs more than the table" (38.8 MB against 34.5 MB on
the large Python repository). It is refused on the format: both directions are needed — the writer resolves key to id,
every reader resolves id to key — so an id-keyed index survives, and a rowid-less table re-stores
its whole primary key inside every secondary index [S8], which would put the full key back into it.
Strictly worse than today.

**Why this wins.** It is the only shape that leaves one b-tree and one copy of each key. Safety is
**detection, not improbability**: the insert compares the stored key against the incoming key and
probes the next id on disagreement, reusing the read-back the interner already performs.

**Trade-off accepted.** Ids stop being dense and sequential, and a collision costs an extra probe.

**Measured after implementation, and it qualifies the projection.** Ids stop being
dense, so every `native_key_id` reference widens from a 1–2 byte record field to an 8-byte one, and
the dictionary's own rowid b-tree stops being densely packed. The projection above counted only the
dropped automatic index and missed both costs. On a small fixture store (12 233 distinct keys,
19 253 evidence rows, 12 274 alias rows) the automatic index's 618 496 B goes, but `native_keys`
grows 552 960 → 696 320 B, `evidence` 1 957 888 → 2 080 768 B and `native_aliases` 204 800 →
286 720 B: **a net 131 072 B larger store**. The saving scales with the number of distinct keys and
the cost with the number of references to them, so the shape wins only where keys are many relative
to references — on the large Python repository's figures (≈201 k keys against ≈917 k references) it still projects a
net ≈30 MB saving rather than the ≈38 MB claimed above. A narrower digest (fewer bits, more probes,
the same detection) would keep the saving at both ends and is the obvious follow-up; it is not
adopted here because it was not the option this record decided.

### 2.4 The content store keeps plaintext objects

An object is the blob's plaintext, uncompressed, published under its SHA-256. A range read is one
positioned read of the 64 KiB blocks that cover it, each checked against its block digest; a stream
reads the file in order, checking each block and then the whole-file digest; the already-published
check is one `lstat` that the size matches. No read needs a decode buffer or decode CPU, and no put
needs an encoder, a second surface or a frame index. The content tier is therefore exactly 1.0×
eligible source.

**Chosen by measurement, for speed and memory.** Disk is not a criterion for this tier: the choice
is the faster form, and plaintext on a tie. Three forms were built in the store and measured through
its own entry points: plaintext; S2 default [S18] and Zstandard default [S17] [S19], each as
independent per-block frames under a frame index in the object, the seekable framing [S9] with the
block-gzip precedent for random access [S10]. Through the store, on 1 985 files of Go source and
assets (21.6 MB), three alternating runs per form, 4 threads, warm page cache except the cold columns
(objects evicted with `posix_fadvise(POSIX_FADV_DONTNEED)` [S22] before the pass):

| form | stored / plaintext | first write (CPU) | write allocation | stream, warm | range read (2 KiB), warm | stream, cold | range read, cold |
|---|---|---|---|---|---|---|---|
| **plaintext** | **99.8 %** | **450–510 ms (298–349 ms)** | **130 MiB** | **533–599 MB/s** | **7.7–9.3 µs** | 62–73 MB/s | 79–96 µs |
| S2 framed | 33.5 % | 596–664 ms (435–464 ms) | 277 MiB | 172–326 MB/s | 14.1–16.5 µs | 58–69 MB/s | 89–96 µs |
| Zstandard framed | 23.8 % | 667–811 ms (476–584 ms) | 385 MiB | 296–334 MB/s | 21.0–22.9 µs | 48–64 MB/s | 92–193 µs |

Peak resident memory of the probe process was 32–35 MB for every form. Plaintext is faster on every
read and write column; cold reads are within run-to-run noise of S2, because a cold read of a source
file costs a device access per object, not per byte. Through the whole product — indexing 622 files
(6.6 MB) end to end, then reading every file back through the chunked source read every session read
uses — the forms are within noise: index 44.0–48.8 s plaintext against 44.7–48.2 s S2, peak resident
944–1 011 MiB against 997–1 026 MiB, 245–266 µs a chunk warm against 266 µs, 455–494 µs cold
against 486–489 µs. The store is a small share of an index run, so a store-level win shows there as
a tie, and a tie is plaintext.

**Precedent.** A production code-search engine keeps file contents uncompressed in its shards and
serves a content slice as a direct read at an offset [S21].

**Alternatives steel-manned.** *S2 frames* store 33.5 % and read nearly as fast as plaintext cold;
refused because they are slower warm (≈1.8× a range read, ≈2× a stream), cost an encode, a second
surface and twice the write allocation, and save only disk. *Zstandard frames* are the smallest at
23.8 %; decode speed does not move with the level [S15], so no level closes the ≈2.5× warm range-read
gap; refused on the same grounds. *Compress the database instead* is not available: there is no
page compression in the public-domain engine build, only a separately licensed extension [S11].
*Raise the page size and vacuum* changes fill and fan-out, not payload, and takes effect only at a
vacuum and not in write-ahead mode [S12]. *Train a dictionary* adds a documented ~10 % at 64 KiB
inputs [S13] and would make the dictionary a retention invariant the collector cannot express.

**Trade-off accepted.** The content tier is 1.0× eligible source rather than ≈0.24–0.34×, and the
§2.5 composite figure carries it.

### 2.5 The gate becomes three budgets, one per tier

Row 13's single ratio is retired as the gate and replaced by three, each measured against the
quantity that tier actually scales with: **content store = 1.0× eligible source** (the plaintext, §2.4), **lexical tier
≤ 0.6× eligible source**, **fact and provenance tier ≤ 2 000 B per indexed symbol**. The composite
figure stays published as their sum, for continuity, not as the gate.

**Alternative steel-manned.** *Keep 3.5× and make the store meet it.* On the projection it would in
fact be met — both real repositories pass 3.5× at the first step and land near 3.0× — so this is not
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
[S16]; the content budget is the plaintext itself, exactly source-sized; the per-symbol budget is derived above
and is ~14× the number it replaces because that number was never derived at all.

**Trade-off accepted.** Row 13 stops being one comparable number across the project's history.

### 2.6 Sequence

1. **Precondition.** The content-store read ceiling a verification run recorded — `byte
   range spans 4292608 bytes, over the 1048576-byte read ceiling`, a bound no user set, which takes
   the whole answer down instead of truncating and flagging — becomes a user-set bound that is
   unlimited by default, and the integrity check is reimplemented. §2.1 must not land first:
   it puts that ceiling on the search path.
2. §2.1 lexical tier. 3. §2.2 evidence identity. 4. §2.3 key dictionary. 5. §2.4 content store.

Projected, each step applied to the one before (content tier uncompressed at 1.0×, so §2.4 moves no ratio):

| step | large Python repository ratio | large Python repository B/symbol | reference repository ratio | reference repository B/symbol |
|---|---|---|---|---|
| today | 4.54× | 3 600 | 4.16× | 3 296 |
| + §2.1 | 3.46× | 2 562 | 3.26× | 2 478 |
| + §2.2 | 3.34× | 2 442 | 3.14× | 2 370 |
| + §2.3 | 3.11× | 2 222 | 2.95× | 2 195 |
| + §2.4 | 3.11× | 2 222 | 2.95× | 2 195 |

---

## 3. Consequences

- **Query latency.** The hydrated search page never carried a body, so first-page time is
  unaffected; only a snippet-bearing response pays, one verified block a hit. Every step must be
  measured against first-page time before the next lands.
- **Indexing throughput.** §2.1 removes the largest write and two read-backs and §2.3 removes an
  index write per distinct key; §2.4 adds nothing to the write path, since the surface a put hashes
  into is the object it publishes. A net gain is expected and must be shown, not assumed.
- **Invariants.** No step introduces a bound. Peak resident memory stays a function of the page:
  one verified block a hit, one block at a time in the writer.
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
— independently compressed frames plus a trailing index of compressed and decompressed sizes; the
framing of the two compressed forms measured and refused in §2.4.

[S10] *Sequence Alignment/Map Format Specification* (BGZF, §4.1) —
https://samtools.github.io/hts-specs/SAMv1.pdf — a blocked compressed format with ≤64 KiB blocks and
virtual offsets; the precedent for indexed random access into compressed data that the framed forms
in §2.4 followed.

[S11] *ZIPVFS: Read/Write Compressed Database Files* — https://sqlite.org/com/zipvfs.html — page
compression is a separately licensed extension, not part of the public-domain amalgamation.

[S12] *PRAGMA page_size* — https://sqlite.org/pragma.html#pragma_page_size — a new page size takes
effect only at the next vacuum, and not in write-ahead mode.

[S13] *Dictionary compression of small files (issue 468)* —
https://github.com/facebook/zstd/issues/468 — "Typical gains range from ~10% (at 64KB) to x5 better
(at <1KB)"; the size of the refused dictionary option.

[S15] *Zstandard README benchmark table* — https://github.com/facebook/zstd/blob/dev/README.md —
"Decompression speed is preserved and remain roughly the same at all settings".

[S16] *Regular Expression Matching with a Trigram Index* —
https://swtch.com/~rsc/regexp/regexp4.html — a lexical index at ~20 % of the indexed sources (77 MB
over 420 MB); the external reference point for the lexical-tier budget.

[S17] *Go Zstandard implementation, README* —
https://github.com/klauspost/compress/blob/master/zstd/README.md — the encoder "can be assumed to
produce the same output from the exact same code version" (and no stronger promise), `EncodeAll`
and `DecodeAll` are safe for concurrent use, and the decoder runs without allocations once warm; the
codec of the Zstandard form measured and refused in §2.4.

[S18] *S2 compression, README* — https://github.com/klauspost/compress/blob/master/s2/README.md —
S2 against Zstandard on mixed content (73.6 % against 80.2 % reduction, ≈4× the encode speed) and on
the first 64 KB of each Go source file (44.8 % default, 39.4 % better); the S2 form measured and
refused in §2.4.

[S19] *Zstandard Compression and the 'application/zstd' Media Type* (RFC 8878) —
https://www.rfc-editor.org/rfc/rfc8878 — the frame format: a frame is self-contained, and its
content checksum is optional, which is why the measured Zstandard form ran with it off under the
per-block SHA-256.

[S20] *NIST SP 800-107 Rev. 1, Recommendation for Applications Using Approved Hash Algorithms*
(§4.1, §5.1) — https://nvlpubs.nist.gov/nistpubs/Legacy/SP/nistspecialpublication800-107r1.pdf —
"the expected collision-resistance strength of a hash function is half the length of the hash
value"; a truncated digest takes "the λ left-most bits" and needs λ ≥ 2s for collision resistance
s. The basis of §2.2's adversarial margin and of keeping the prefix rather than any other 16 bytes.


[S21] *Zoekt shard reader* — https://raw.githubusercontent.com/sourcegraph/zoekt/main/index/read.go
— `readContents` and `readContentSlice` return a file's content as a section of the shard at an
offset, with no decode step (`index/write.go` writes the content strings as they are); the
precedent for §2.4's plaintext objects.

[S22] *posix_fadvise(2)* — https://man7.org/linux/man-pages/man2/posix_fadvise.2.html —
`POSIX_FADV_DONTNEED` "attempts to free cached pages associated with the specified region"; how
§2.4's cold columns evicted the objects before a pass.
---

*Every figure in this record is quoted from the verification run's per-b-tree attribution or
derived in [`docs/research/16-storage-tier2.md`](../research/16-storage-tier2.md), where the
arithmetic is shown, except §2.2's and §2.4's measurements, which were taken on the fixtures they name.*

**Implementation note on §2.1.** The record says `body`'s only database readers are the insert and
the two paths that feed the index a delete. There is a third: the delta carry-over copies a previous
unit's lexical documents, and it read `body` back to re-index them at their new rowids. Contentless,
there is nothing to read back — and nothing to re-derive either. A document's body is what its
producer published, not the file's bytes over `[start_byte, end_byte)`: the tree-sitter provider
publishes a symbol's names, signature and attached documentation over the whole declaration's
extent, and the filesystem provider publishes no body at all. Re-indexing a carried document from
the source would therefore index text that was never any document's body, changing that document's
hits, BM25 length and snippets on every delta, with no failing check anywhere — every integrity
check a contentless index offers is internal to the index.

So the carry-over does not rebuild the posting; it **keeps** it. `search_units` carries a `doc_id`
column naming the `search_fts` rowid its text was indexed into, the carry-over copies that column
forward with the rest of the row, and every read resolves a document by `doc_id`. Sharing is
confined to one carry chain — `CarryOver` refuses a predecessor of another provider or scope key,
and `generation_units` is keyed by `(generation_id, provider_id, scope_key)`, so at most one row of
a chain is visible in any generation — and a posting is released, like a `node_ids` row, only when
no surviving unit still names it. The carry-over reads and writes no text at all: one bounded
`INSERT..SELECT`, no content-store read, and a peak set by the statement rather than by the unit.
The "rebuild source becomes the content store" consequence the trade-off names still holds for a
rebuild from scratch; it does not reach the incremental path.
