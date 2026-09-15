# ADR-0002 — Storage identities: integer surrogates and interned keys

- **Status:** Accepted (implemented)
- **Date:** 2026-09-14
- **Refines:** [`ADR-0001 — Scale posture`](ADR-0001-scale-posture.md) §2.8, which accepted and
  scheduled this redesign; this record carries the shape that was built, the alternatives that were
  refused on the way, and the sources the implementation lanes cited
- **Inventory and per-table attribution:** [`docs/research/15-scale-posture.md`](../research/15-scale-posture.md)
- **Budget row:** [`docs/performance.md`](../performance.md) row 13

---

## 1. Context

### 1.1 What was measured, and on which corpus

Three figures drove this decision. They come from three different stores and must be read with
their corpus attached; none of them is a restatement of another.

- **73.86× stored bytes over eligible source bytes** — 420 810 752 B over 5 697 273 B on the
  10 000-file generated corpus, against a 3.5× budget. An earlier run of the same corpus reported
  75.00×. This is `docs/performance.md` row 13's miss.
- **6784.0 bytes per indexed symbol** — the redesign's own baseline, taken with per-b-tree byte
  attribution [S1] over a read-only copy of the `cache-wave-g2` fixture store: 587 776 000 database
  bytes (`page_size` 4096, `page_count` 143 500, 51 free pages, no write-ahead log bytes
  outstanding) over 86 642 `node_facts` rows.
- **≈3.4 KB per indexed symbol** on the 604-file cut of the same generator, the figure ADR-0001
  §2.8 quotes.

Bytes per indexed symbol is the corpus-independent metric, because the cost is per identity: the
same symbol count spread over more source moves the *ratio* and not the per-symbol number.

### 1.2 The amplifier is identity width, not a configured bound

No bound in the scale-posture inventory is causal here. Every count and size bound in the store is
already user-set and unlimited by default, and the amplification is the same at every corpus size,
which is what rules a bound out as the cause.

What the per-b-tree attribution shows instead is a 32-byte canonical identity replicated into every
table that references it and then again into every index built over those tables. On the fixture
store the largest b-trees are evidence at 95 457 280 B, `idx_alias_node` at 41 684 992 B,
`idx_alias_lookup` at 40 312 832 B, `native_aliases` itself at 39 989 248 B and `relation_ids` at
33 849 344 B — two alias indexes together costing more than twice the table they index, over
274 807 rows. The `fact_keys` family, which ADR-0001 could not size because its corpus had zero
rows in it, is 62 173 184 B here over 204 887 rows: **10.6 % of the whole file**, closing that
unknown.

The mechanism is in the file format, not inferred: a record field holding a 32-byte blob costs
about 33 B including its serial type varint [S2], an index b-tree key is the indexed columns
followed by the table's row key — which for a table declared without a rowid is the *whole* primary
key [S2][S3] — and a single-column integer primary key is the b-tree key itself and therefore costs
zero payload bytes [S4]. A rowid-less table with a wide primary key and any secondary index is an
amplifier by construction, and the format's own guidance already excludes the shape [S3].

The wide text keys amplify the same way. On the fixture store `native_aliases` carries 274 807
scope-key values drawn from only **403 distinct** strings (average 34 B), and 216 385 distinct
native keys (average 52 B), each replicated three times — table, `idx_alias_lookup`,
`idx_alias_node`. `evidence` adds 477 388 native-key copies drawn from the same vocabulary, of
which 52 711 are distinct (average 53 B).

---

## 2. Decision

### 2.1 Surrogate row identities, canonical identity stored once

`node_ids` and `relation_ids` become rowid tables: `id INTEGER PRIMARY KEY CHECK(id > 0)`, with the
canonical 32-byte identity kept exactly once as
`canonical BLOB NOT NULL UNIQUE CHECK(length(...) = 32)`. Every reference site — `node_facts.node_id`, `relation_facts.relation_id`, `relation_ids`'
endpoints, `native_aliases.node_id`, `fact_keys`' two nullable refs, `evidence`' two nullable refs,
`search_units.node_id`, `context_entries.node_id` — becomes `INTEGER`.

`node_ids.canonical_key` becomes `BLOB(32)`, having been 64-character lowercase hex TEXT stored
twice (in the table and in its unique auto-index). The blob ordering is byte-for-byte the old
lowercase-hex TEXT ordering, proven on rows including a high first byte and a tie broken by the
canonical identity, so the reconcile keyset resumes exactly where it did before.

The constant `repository_id` column is dropped **from those two identity tables only** — one store
is one repository — and from their unique constraints. `snapshots.repository_id` and
`generations.repository_id` are untouched.

`node_facts`, `relation_facts` and `native_aliases` keep their rowid-less shape; what changes is
that their primary keys are now varints rather than 32-byte blobs, which is what makes every index
built over them cheap.

### 2.2 Two string dictionaries, one shared vocabulary

`scope_keys(id INTEGER PRIMARY KEY, key TEXT UNIQUE)` and `native_keys(id INTEGER PRIMARY KEY,
key TEXT UNIQUE)` hold the wide text keys once. `native_aliases` becomes all-INTEGER —
`(unit_id, scope_key_id, native_key_id, node_id)` — and `evidence.native_key` becomes
`evidence.native_key_id`. This is the standard dictionary split that mature text indexes have
always used: strings map to ordinals in a dictionary and only ordinals appear in the bulk
structures [S5].

`evidence`'s native keys come from the same vocabulary as the aliases', so they share the
`native_keys` dictionary rather than getting one of their own.

**The empty native key is valid.** Evidence may carry no native key and the column is `NOT NULL`,
so unlocated evidence interns the empty string like any other value. The empty *scope* key stays an
error.

### 2.3 Interning is a bounded cache, never a repository dictionary

The writer resolves strings and identities through a bounded least-recently-used cache held on the
unit writer, sized from the writer's batch sizing (`batch_records`, default 1000, floored at 256)
and **emptied at every write-batch boundary**. Peak is roughly three caches × capacity ×
(key + ~80 B) — under 1 MiB at the default — and is a function of batch size, never of repository
size. A miss is one indexed lookup against a unique constraint, never a rescan.

Resolution is upsert-then-read inside the caller's own write transaction, so two writers converge
on the same row. `RETURNING` was rejected for the purpose: it returns only rows that are directly
modified [S6], and the case that needs the identity is precisely the suppressed insert, which
modifies nothing. The last-inserted-rowid accessor was rejected for the same reason — a suppressed
insert leaves it pointing at an earlier row [S7]. The upsert uses a catch-all conflict clause,
because both identity tables carry two uniqueness constraints and an omitted target fires on any of
them [S8]; a read-back that then finds no row means a *different* canonical identity already owns
that tuple, which is a caller bug and is reported as a typed error rather than retried.

### 2.4 Surrogates are storage-internal; canonical identities are hydrated at the boundary

No surrogate crosses a package boundary. The reader joins outward and projects `canonical` in place
of the surrogate, so the stored node, relation, evidence and search-document shapes are unchanged
byte-for-byte and the consuming packages needed no edit at all. Every cursor payload carries the
canonical identity — node, relation, evidence, file — because a surrogate is meaningful only inside
one store and one rebuild.

Hydration sits **outside** the keyset-bounded inner select. The batched-edge read is the shape that
shows it: an inner select bounded by the cursor and the page limit, wrapped by an outer select that
probes the identity tables for the rows it actually returns. The dictionary probes therefore run
once per returned row, never once per candidate.

### 2.5 `fact_keys` uniqueness, re-specified

The old unique index used a blob sentinel (`coalesce(node_id, x'')`) over two columns that are now
INTEGER. No sentinel is needed: the table check guarantees exactly one of the two refs is non-NULL,
and NULLs are distinct in a unique index [S9]. Two plain unique indexes enforce exactly the old
constraint over narrower keys *and* subsume the two `(unit_id, <ref>)` probe indexes the delta path
used — three indexes become two. Partial indexes were tried and rejected: a partial index is usable
only where the statement's `WHERE` clause implies its predicate [S10], and under the partial
variant the unit-deletion cascade `DELETE FROM fact_keys WHERE unit_id = ?` degrades from an index
seek to a table scan.

### 2.6 Migration is a rebuild

The schema fingerprint is a digest of the embedded DDL, so it bumps by construction
(`f0f6573d…` → `4748ed37a98d3bd60e95565eab2cebf12e250ebf0df3ce87847305a57b889143`). An existing
store fails closed with `CTX_SCHEMA_MISMATCH` and an explicit rebuild decision. There is no
migration layer and no dual read path.

---

## 3. Alternatives considered and refused

**Reordering the alias table's primary key to drop one of its indexes.** Four separate sweep paths
filter `native_aliases` on the leading `unit_id` column, so the reorder turns the unit-deletion
cascade into a table scan; adding `(unit_id)` back as its own index re-stores the whole primary key
and returns the bytes it saved. Refused. Interning is what makes those indexes cheap instead.

**Converting the alias table to a rowid table with a unique auto-index.** Measured net-neutral,
5.07 → 4.96 MB. Refused as churn.

**S-6 — making `evidence.id` a plain rowid (≈ −13.5 MB projected).** **Dropped, not forced**, and
by call site rather than by size. The unit-seal and carry-over writers both upsert with
`INSERT … ON CONFLICT(id) DO NOTHING` (`units.go:747`, `delta.go:435`), and a conflict target
resolves only against a primary key or a unique index [S8] — so dropping it is a statement-prepare
failure on every seal, not a slow path. That alone settles it. Three further facts confirm it: the
surplus-evidence clip deletes **by id value** (`delta.go:573`), the id is on the wire as the
evidence identifier on reference occurrences and fact references, and the capsule's content hash
folds those values. The id is a pure function of the evidence fields, so the uniqueness it enforces
is a derived invariant rather than an arbitrary surrogate. The node and relation refs on the table
still become INTEGER.

**S-7 — evidence as a compressed set rather than one row per fact→unit link.** Deferred: it is a
second-order change against a first-order one, and it is worth attempting only if the surrogates
and the dictionaries miss the gate. ADR-0001 §2.8 records the published analogue.

**Truncating canonical identities to 16 bytes.** Not proposed. It would change canonical identities
and therefore determinism. It is recorded in ADR-0001 §2.8 as the next lever if the redesign
misses, not as part of this one.

**A separate `paths` intern table.** Refused: the only high-multiplicity path column is
`search_units.path`, and the full-text index declares it as an external-content column [S11], so it
must stay a TEXT column under that name or the path field leaves the full-text index. The other
path column is 818 rows / 114 KiB on the fixture and is not an amplifier. The alternative was a
table with no writer.

**Dropping the outgoing-relation lookup index.** Done, and it is a refusal of the *opposite* kind:
after `repository_id` left `relation_ids`, its unique constraint is column-for-column the same
index, and the query plans are identical — so the explicit index was redundant rather than merely
cheap (−21.4 MB on the fixture).

**Sweeping unreferenced `native_keys` rows in this change.** Not shipped. Both child columns are
unindexed — the alias lookup index carries `native_key_id` second and evidence has no index on it —
so the sweep degrades to a full pass over the evidence table *per candidate*, inside the write
transaction, which is the exact pathology this decision exists to remove. Shipping the two indexes
that would fix it means putting a second index on the largest table in the store in a change whose
purpose is shrinking it, against roughly 52 711 reclaimable dictionary rows. Recorded as a measured
trade-off to settle with numbers, not as an oversight; unreferenced `scope_keys` *are* swept, in
bounded batches, and the `native_keys` sweep is a copy of that statement once the index decision is
made.

---

## 4. Consequences

**What shrinks.** Every reference site drops from ~33 B to a 1–4 byte varint, the identity tables
stop storing a 32-byte primary key that each of their secondary indexes re-stored, the hex identity
column halves, a constant column disappears from two of the largest tables, one redundant index is
gone and the `fact_keys` index family goes from three indexes to two. The wide text keys are stored
once each instead of three and four times respectively.

**What the reader pays.** One dictionary probe per returned row, and only per returned row: the
hydration is an outer select over the keyset-bounded, limit-bounded inner select. Across the full
before/after plan matrix no table is scanned on either side; the one added `SCAN` line is over a
materialised co-routine — the outer hydration reading at most a page of rows — which is the point of
the shape. Two read paths move their keyset from a fact table onto the canonical identity column,
because a cursor may carry only the canonical identity; both remain unit-bounded and page-bounded.
The alias lookup's identity probe becomes a rowid seek where it was a unique-index probe.

**Determinism is preserved by construction.** Canonical hashes exclude operational identifiers, so
a row-id surrogate is the correct shape rather than a tolerated one. The canonical identities still
drive the analysis key, the unit identity and the manifest identity; carry-over re-derives evidence
identities by joining back out to `node_ids`, `relation_ids` and `native_keys` for the canonical
values. Surrogate *values* depend on insert order, which is exactly why they never leave the store.

**Migration is a rebuild, and only a rebuild.** An existing store is refused with
`CTX_SCHEMA_MISMATCH`; there is no upgrade path and none is offered.

**Retention accounting reports smaller figures**, deliberately. The interned columns no longer
exist as per-row text, and they are not replaced by a join back to the dictionary: a dictionary row
is shared by every generation that used the key, so charging it to one generation would claim as
reclaimable bytes that deleting that generation cannot free.

**Still open, recorded so it is not rediscovered.** `evidence.detail` is a further intern candidate
(668 distinct values over 477 388 rows, ≈2.4 MB). `fact_keys.fact_key` is left TEXT although every
value on the fixture is a 64-character hex digest (≈13 MB if converted); converting it needs a
model-level guarantee that the producer's key is always a digest, which is a provider contract
change and not a storage one. The unit-hash text columns are left TEXT at 0.013 % of the file. And
the ratio gate itself is unsettled: ADR-0001 §2.8's honest projection lands near ≈24× on the
original corpus, so the gate must be re-measured on the corrected reference corpus and on real
repositories before row 13 is re-recorded.

---

## Measurements

Measured by `scripts/dbstat-baseline.sh` on two stores built from the **same**
4 019-file repository by two binaries — the pre-redesign one and this one —
each with `[tools] offline = true`, an isolated `XDG_CONFIG_HOME`/`XDG_CACHE_HOME`
and the SCIP, LSP and dependence providers disabled. Both stores report
**identical row counts** in every identity table (node_ids 140 519, node_facts
175 461, relation_ids 264 735, relation_facts 279 859, native_aliases 387 511,
evidence 643 025, search_units 73 009, files 4 035, units 6 850), so the whole
difference below is representation, not corpus.

| | before | after | change |
|---|---|---|---|
| store bytes | 1 005 887 488 | 632 266 752 | **−373 620 736 (−37.1 %)** |
| **bytes per indexed symbol** | **5 732.8** | **3 603.5** | **−37.1 %** |

Both totals are **pre-narrowing**: the after store was built before `idx_nodes_file`
lost its dead middle column, and that index's −1 093 632 B is measured on a copy of
the same store further down. The two savings are not additive with the headline.

Per b-tree, largest movers (bytes):

| b-tree | before | after |
|---|---|---|
| `idx_alias_node` | 85 397 504 | 7 725 056 |
| `idx_alias_lookup` | 80 670 720 | 7 729 152 |
| `native_aliases` | 80 494 592 | 7 720 960 |
| `evidence` | 100 933 632 | 67 084 288 |
| `relation_ids` (+ its two autoindexes) | 88 350 720 | 33 607 680 |
| `node_ids` (+ its two autoindexes) | 39 759 872 (+`sqlite_autoindex_node_ids_1`) | 25 579 520 |
| `idx_relations_from` + `idx_relations_to` | 49 938 432 | 6 709 248 (one index) |
| `node_facts` | 31 604 736 | 25 718 784 |
| `idx_evidence_relation` / `_node` | 20 946 944 / 19 820 544 | 9 854 976 / 9 633 792 |
| `native_keys` + its autoindex | — | **73 412 608 (new)** |

The dictionary is the one new cost: `native_keys` and its `UNIQUE(key)` autoindex
hold 402 568 distinct strings twice, 73.4 MB, against the ~180 MB the same strings
cost when replicated per alias and per evidence row.

Two numbers this corpus does **not** settle. `fact_keys` has **0 rows** in a
single-generation full index — the delta path is what populates it — so the
`fact_keys.fact_key` sizing stays the fixture's 204 887 rows. And the
source-bytes ratio is S-VERIFY's to report, not this one's.

### `native_keys` sweep (progress-ledger ruling (b))

The two indexes the sweep needs, built on the after store and measured:
`idx_alias_native ON native_aliases(native_key_id)` = 6 799 360 B and
`idx_evidence_native ON evidence(native_key_id)` = 7 352 320 B, together
**14 151 680 B = 2.24 % of the 632 266 752 B store** — over the 2 % line the
ruling drew. With them the batched predicate is fully indexed
(`SEARCH na USING COVERING INDEX idx_alias_native`, `SEARCH e USING COVERING
INDEX idx_evidence_native`); without them each candidate costs a full pass over
643 025 evidence rows. Per the ruling the indexes are therefore **not added**
and the sweep is owed as a bounded batch job per retention run; it is not
implemented here.

### Index re-audit (§3c), against `EXPLAIN QUERY PLAN` on the after store

`idx_search_unit ON search_units(unit_id, rowid)` — **KEPT, with the reason the
audit asked for.** `delta.go:419` pages the rows a unit just wrote by rowid:
`SEARCH search_units USING INDEX idx_search_unit (unit_id=? AND rowid>?)`.
`UNIQUE(unit_id, search_key)` cannot serve that seek; the index is 1 187 840 B.

`idx_nodes_file` — **NARROWED** from `(file_id, start_byte, unit_id)` to
`(file_id)`. `start_byte` was dead: the only range over it is
`coalesce(nf.start_byte, 0)`, which no plain column index can serve, and
`unit_id` is already in the primary-key suffix every index key carries. Both
consumers keep their plan on the shipped projections:

```
NodesInFile (real column list, keyset and ORDER BY as shipped)
  3-column  |--SEARCH nf USING INDEX idx_nodes_file (file_id=?)
            |--SEARCH ni USING INTEGER PRIMARY KEY (rowid=?)
            `--USE TEMP B-TREE FOR ORDER BY
  narrowed  identical, line for line
gc.go:104   SELECT 1 FROM node_facts nf WHERE nf.file_id = ?
  3-column  `--SEARCH nf USING COVERING INDEX idx_nodes_file (file_id=?)
  narrowed  `--SEARCH nf USING COVERING INDEX idx_nodes_file (file_id=?)   [still COVERING]
```

5 328 896 B → 4 235 264 B, **−20.5 %**, no plan changed.

---

## Sources

Every URL cited by the implementation reports behind this decision appears here, once, with what it
was used for. Body citations are by `[Sn]`.

[S1] https://www.sqlite.org/dbstat.html — per-b-tree byte attribution in aggregate mode
(`SUM(pgsize) GROUP BY name`); the method behind the baseline and every per-table number in §1.

[S2] https://www.sqlite.org/fileformat2.html#record_format — record format and serial types: a
32-byte blob field costs `2N+12` plus its varint (≈33 B), text `2N+13`; an index key is the indexed
columns followed by the table's row key.

[S3] https://www.sqlite.org/withoutrowid.html — a rowid-less table's index key re-stores the whole
primary key, plus the explicit guidance against wide string and blob keys; the shape the alias
table violated and the reason its primary-key reorder was refused.

[S4] https://www.sqlite.org/lang_createtable.html#rowid — a single-column integer primary key *is*
the row id: zero payload bytes, and the fastest available lookup.

[S5] https://lucene.apache.org/core/10_0_0/core/org/apache/lucene/codecs/lucene90/blocktree/package-summary.html
— block-tree term dictionary: a string→ordinal dictionary with ordinals only in the postings; the
established form of the interning in §2.2.

[S6] https://www.sqlite.org/lang_returning.html — `RETURNING` (available since 3.35.0) returns only
rows that are *directly modified*, so a do-nothing upsert returns nothing; why the interner reads
back instead.

[S7] https://www.sqlite.org/c3ref/last_insert_rowid.html — the last-inserted-rowid accessor is
unchanged by an insert that does not insert; the second reason for the unconditional read-back.

[S8] https://www.sqlite.org/lang_upsert.html — an upsert conflict target resolves only against a
primary key or a unique index, and an omitted target fires on any uniqueness constraint; the basis
of both the `evidence.id` refusal (§3) and the catch-all interner upsert (§2.3).

[S9] https://www.sqlite.org/lang_createindex.html#unique_indexes — NULLs are distinct in a unique
index, and a unique constraint creates an automatic index; why the `fact_keys` sentinel was
unnecessary and why two identity constraints already carry their own indexes.

[S10] https://www.sqlite.org/partialindex.html — a partial index is usable only when the statement's
`WHERE` clause implies the index predicate; why the partial `fact_keys` variant was rejected.

[S11] https://www.sqlite.org/fts5.html#external_content_tables — external-content full-text tables
and their `content_rowid` contract; why the search path column must stay TEXT under its own name
and why the search row-id cursor has no canonical counterpart to carry.

[S12] https://www.sqlite.org/queryplanner.html — the query planner and covering indexes; the frame
for the call-site-driven index audit.

[S13] https://www.sqlite.org/eqp.html — `EXPLAIN QUERY PLAN` output grammar (SEARCH versus SCAN,
CO-ROUTINE, temporary b-trees); how the before/after plan matrix in §4 is read.

[S14] https://www.sqlite.org/optoverview.html#the_analyze_command — `ANALYZE` and the default
selectivity used when no statistics exist; why the before/after comparison was taken on
un-analysed stores, where a SCAN means no usable index exists at all.

---

*Every figure in this record is drawn from the implementation and verification reports of
2026-09-14, each of which carries the plan output, byte attribution or mutation proof for the
assertion it supports.*
