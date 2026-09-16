# Storage: delta imports

`internal/storage/sqlite` is the Section 12 store. This document covers the
two things the DDL of Section 12.2 does not explain on its own: how identities
are actually stored — as row surrogates and interned keys, with the canonical
identity hydrated back at the response boundary — and how a provider that can
tell what changed since its last run seals a new unit as "the previous unit
plus a delta" (Section 11.4) without weakening any of the guarantees a sealed
unit carries.

Everything else about the store — the single writer, generation activation,
retention by distinct ref — is Sections 12.2–12.4 of
`docs/implementation-plan.md` and is not restated here.

## How identities are stored

Node and relation identities live in two dictionary tables, `node_ids` and
`relation_ids`. Each has an `INTEGER PRIMARY KEY` surrogate and stores the
canonical 32-byte identity once per row rather than replicating it at every
reference site, in `canonical BLOB NOT NULL UNIQUE` under a
`CHECK(length(canonical) = 32)`. Every
other table references a node or a relation by that **integer** surrogate:
`node_facts`, `relation_facts`, `relation_ids`' own two endpoints,
`native_aliases`, `fact_keys`, `evidence`, `search_units` and
`context_entries`. `node_ids.canonical_key` is a `BLOB` under a
`CHECK(length(canonical_key) = 32)` — not the 64-character hex text it used to
be, and unique only in combination, through `UNIQUE(kind, canonical_key)` — and neither identity table carries a
`repository_id` column any more, because one store is one repository.
(`snapshots` and `generations` still carry theirs.)

The two wide text keys are interned the same way. `scope_keys(id, key)` and
`native_keys(id, key)` hold each distinct string once; `native_aliases` is
all-integer — `(unit_id, scope_key_id, native_key_id, node_id)` — and evidence
carries `native_key_id`. Evidence and aliases share `native_keys`: the values
come from the same vocabulary.

The two dictionaries allocate their ids differently. `scope_keys` keeps
`key TEXT UNIQUE` and resolves key to id through that index. `native_keys` does
not: its automatic index over `key` cost more than the table it indexed (38.8 MB
against 34.5 MB on one reference store), because interning stored every distinct
key twice. It is **hash-keyed** instead — the id is the first 63 bits of
`SHA-256(key)`, zero clamped to 1 — so the writer computes the id without a
lookup and one b-tree holds one copy of each key. Both directions still work:
a reader joins id to key through the primary key, and a writer or the alias
reconciler resolves key to id by walking that key's chain, comparing the stored
key at each id and probing the next on disagreement. **Two keys whose digests
agree are detected and separated, never merged**; the comparison is the safety
property, not the improbability of a collision.

Three properties follow, and each is load-bearing for the rest of this
document.

- **The surrogate never leaves the store.** `model.NodeID`, `model.RelationID`
  and `model.EvidenceID` on the wire are the canonical identities, and a signed
  cursor encodes the canonical identity too — a surrogate is meaningful only
  inside one store and one rebuild. Every read joins the identity tables back
  out and projects `canonical`, so the row shapes the reader returns are
  unchanged. That hydration sits *outside* the keyset-bounded inner select, so
  the dictionary probes run once per returned row and never once per candidate.
- **The writer's dictionary is bounded.** Strings and identities are resolved
  through a least-recently-used cache sized from the writer's batch sizing and
  emptied at every batch boundary — never a whole-repository dictionary in
  heap. A miss is one indexed lookup against a unique constraint. Resolution is
  upsert-then-read inside the caller's own write transaction, so two writers
  converge on the same row.
- **Determinism is untouched.** Canonical hashes exclude operational
  identifiers, so surrogate values — which depend on insert order — cannot
  reach one. Carry-over re-derives evidence identities from canonical values,
  joining back out to `node_ids`, `relation_ids` and `native_keys` to recover
  them.

There is no migration path onto this schema and none is offered: the schema
fingerprint is a digest of the DDL, so it changed, and an older store is
refused at open with `CTX_SCHEMA_MISMATCH` and an explicit rebuild decision.

The reasoning behind the redesign, the alternatives that were refused and the
sources are in
[ADR-0002 — Storage identities](adr/ADR-0002-storage-identities.md).

## Why a refresh is a new unit

A unit identity folds its input hash, so a refresh over edited source is
always a *new* unit, never a mutation of the old one. "Previous unit plus a
delta" therefore means: a new unit whose facts are the previous unit's
surviving rows plus the freshly imported ones. The previous unit stays exactly
as it was, retained under `retain_refs` and collected by the ordinary rules.

## Copy, not row sharing

`UnitWriter.CarryOver` **copies** the surviving rows into the new unit. It does
not point the new unit at the old unit's rows.

Sharing was rejected. `deleteUnit` (Section 12.4) is the single unit-deletion
procedure and deletes a unit's facts by `unit_id`; every query and every
membership check reaches facts through `generation_units`. Under sharing, all
of that would have to learn about unit chains, a retired predecessor could not
be collected while any successor lived, and discarding a failed refresh would
delete rows a published generation was serving. The copy costs one bounded
`INSERT .. SELECT` per fact table and leaves queries, membership and collection
untouched.

The copy is also cheaper than the re-import it replaces: nothing is decoded,
resolved or validated against the snapshot, and `search_units.token_count` is
copied rather than recomputed. The carried documents' text is not copied at
all, and not re-derived either. The database holds no body (below), and the
source over a document's byte range is not the body its producer published —
the tree-sitter provider publishes a symbol's names, signature and attached
documentation over the whole declaration's extent, and the filesystem provider
publishes no body — so rebuilding the index from the file would index text that
was never any document's body and silently change that document's hits, BM25
length and snippets on every delta. Instead `search_units` carries a `doc_id`
naming the `search_fts` rowid its text was indexed into; the carry-over copies
that column forward and the carried document keeps its posting. Reads resolve a
document by `doc_id`, sharing is confined to one carry chain (`CarryOver`
refuses a predecessor of another provider or scope key, and `generation_units`
admits one unit per `(generation, provider, scope)`), and a posting is released
only when no surviving unit still names it — the same reference test that
retires a `node_ids` row. The whole carry-over reads and writes no text.

## The lexical index keeps no second copy of the source

`search_units` has no `body` column and `search_fts` is a **contentless** FTS5
index (`content=''`, `contentless_delete=1`), which is what keeps `DELETE` and
`INSERT OR REPLACE` available when a unit is invalidated. The indexed body text
therefore exists only as index postings; the source itself lives once, in the
content store, and every answer that needs the text — snippets, highlights, the
hydrated window around a hit — reads it there through the verified range reader,
over the `file_id` and byte range each document row already carries. Generic
results still carry no body (Section 14.2); this is about where the bytes are
kept, not about what a result returns.

Two consequences are worth stating plainly. First, **the index's rebuild source
is the content store, not the database**: the index can no longer be
reconstructed or cross-checked from the database alone, so the content store's
durability is now the lexical tier's durability as well. Second, the deep
integrity check (`Store.Check`) proves the index is internally consistent and
that no indexed document outlives its `search_units` row; it cannot compare the
index against a stored copy of the text, because there is none — the text's own
authority is the content store, which verifies every block digest on read.

## Discarding a unit: two costs, two paths

`UnitWriter.Fail` unwinds a building unit synchronously: the FTS delete, every
fact table, and then the identity sweeps that ask, per candidate node, whether
anything still references it. That is proportional to the unit's output, not to
the failure, and for a large unit it is seconds to minutes of work that cannot
be cancelled (it runs under `context.WithoutCancel` so a stop cannot leave a
half-discarded unit).

A build the caller cancelled therefore takes `UnitWriter.Abandon` instead: one
`UPDATE` marking the unit `failed`. The rows stay, and stay invisible — every
read joins `generation_units`, and an abandoned unit is a member of no
generation — until `collectUnreachableUnits` removes them from the next
`Store.Recover` or a retention sweep. A non-cancel failure keeps the cascading
`Fail`: nobody is waiting on the process, and the store is left minimal.

`collectUnreachableUnits` decides "nothing is still writing this" from the
origin run's status rather than the unit's state, because a unit is only ever
written while its origin run is running. That also covers the unit a process
killed mid-build, whose generation may already be gone (`provider_runs.
generation_id` is `ON DELETE SET NULL`), which the generation-scoped `Abort`
query cannot see.

The identity sweep is indexed on both sides it interrogates
(`idx_alias_node`, `idx_context_entries_node`); without them each candidate
node costs a full scan of `native_aliases`, which is quadratic in the unit
being deleted.

The two intern dictionaries are collected by the same tail. A dictionary row
outlives every unit that referred to it — deleting a unit removes its alias and
evidence rows, never the interned string — so without these passes a store that
is rebuilt repeatedly accumulates strings nothing can reach. Each pass is a
keyset over the dictionary's `id` in batch-sized transactions, and each
candidate is proved unreferenced by indexed probes: `idx_alias_lookup` for
scope keys, `idx_alias_native ON native_aliases(native_key_id)` and
`idx_evidence_native ON evidence(native_key_id)` for native keys. Those two
indexes are also what SQLite uses to enforce the two foreign keys onto a
deleted `native_keys` row; without them that check alone is a full scan of both
child tables per deleted row, which is what would make the dictionary
uncollectable in practice. The native-key pass carries a rows-examined budget
as a safety bound: it defers, never skips, so a key a run does not reach is
still unreferenced on the next one.

## The two granularities

Two importers produce deltas at two different granularities, and
`Replaced` expresses both.

| Producer | Delta unit | What the applier names |
|---|---|---|
| `internal/provider/scip` | one SCIP document | `Replaced.Files` (the changed and removed paths' `FileID`s) and `Replaced.Scopes` (their alias scopes) — **shipped**; the applier sketch below is against the code as it stands |
| `internal/provider/dependence/neo4jcsv` | one fact | `Replaced.Keys` (the changed and removed fact keys) — shipped: `emitNodes`/`emitRelations` hand every key behind each fact to `PutKeyedNodes`/`PutKeyedRelations` (`fact_keys` holds one row per key), and `KeySet.Diff` yields the replaced set; the applier that passes it to `CarryOver` is `internal/index/delta` — shipped. |

`Replaced` names the **complement** — what does *not* survive — rather than the
survivors. For both importers that is a handful of entries against tens of
thousands: a one-file edit of this repository replaces 1 of 174 SCIP documents,
and an unchanged engine export replaces 0 of 77,701 fact keys.

All three sets are `iter.Seq` streams, consumed once and never materialized.
The replaced set is small for the refreshes that motivate a delta, but a branch
switch, a regenerated code tree or a first refresh after a formatting pass
leaves *no* document unchanged, and the set is then every document in the
repository — which Section 6 forbids holding in the Go heap. `stageReplaced`
validates each element as it stages it, so the only materialization is
SQLite's own bounded temp table. A stream that fails mid-walk must be reported
by the applier: it staged fewer entries than the applier named, so the
applier checks its stream error *before* `CarryOver`'s own return.

A producer's stream may itself read the store — the dependence applier's
`Files` is a merge join against `Store.UnitInputs` — and `CarryOver` drains all
three streams inside its own write transaction on the single writer
connection. Such a stream therefore takes a reader-pool connection
(`read_connections`, default 2) once per page of its own scan while the write
transaction stays open. WAL readers never wait on the writer, so it cannot
deadlock, but a saturated reader pool stalls the carry-over and the open write
transaction blocks every other writer in the process for the length of the
walk. A producer whose stream reads the store keeps those reads bounded and
proportional to the unit it replaces; the nesting is documented on `Replaced`,
`CarryOver` and `UnitInputs` so a third producer does not discover it by
measuring a stall.

### The retention bucket

A fact's retention bucket is the `FileID` on its evidence. Evidence with a
`FileID` belongs to that path's bucket; evidence with none belongs to the
unit's single index-level bucket. A changed or removed path's bucket is
replaced; every other bucket is inherited.

`Replaced.IndexLevel` drops the index-level bucket. A provider that
republishes every unlocated fact on every run sets it. The SCIP importer does
**not**: under a delta it republishes external symbols only for the documents
it reparsed, so dropping that bucket would leave edges carried from untouched
documents without endpoints, and the unit could not seal.

### The fact keys

`fact_keys(unit_id, node_id, relation_id, fact_key)` holds the producer's own
id-independent keys, supplied through `PutKeyedNodes` / `PutKeyedRelations`
(`provider.DeltaSink`). Storage never derives or interprets one.

A key is needed because a path bucket cannot express every removal. A
dependence edge can disappear while every file holding its evidence is
unchanged — the edit was in the *callee's* file, the evidence is in the
caller's — so only the producer can say the fact is gone. It cannot say so by
fact identity either: a `RelationID` is derived from the resolved endpoints,
which an edit changes, so the removed fact has no identity to name.

**One fact is backed by every key that produced it, not by one.** A canonical
edge is published once and derived from N occurrences, each with its own key,
and the emitter re-emits the whole fact as soon as one of those keys changes.
That is why the keys live in a side table and why `PutKeyedNodes` and
`PutKeyedRelations` take `keys [][]string`, parallel to the facts: `keys[i]`
is every key backing `facts[i]`, at least one, each in practice a lowercase hex
digest — a convention the producers keep but that no model type enforces, which
is why the column stays TEXT (see ADR-0002, Measurements),
sorted and without duplicates.

Folding a fact's keys into one was considered and rejected: `Replaced.Keys`
must name the **previous** unit's keys, and the previous run's grouping of
occurrence keys per fact is not recoverable from what a producer persists
(`neo4jcsv.KeySet` is a flat sorted file of bare digests). A folded key would
also change whenever any occurrence moved, so every such fact would be
reported replaced and re-added — defeating the delta in exactly the
cross-file-edge case the key exists for.

**Carry unless any key is replaced.** A fact is inherited unless ANY of its
keys is in `Replaced.Keys`. That is the same condition the producer re-emits
on, so the fresh and carried sets partition exactly: carrying on "some key
survives" would keep a stale copy beside the fresh one, and dropping only on
"every key replaced" would do the same. A carried fact carries its keys with
it, or the successor would hold facts no later refresh could replace.

Facts written with no keys have no `fact_keys` row and are never key-excluded;
only their bucket can replace them. If the applier names replaced keys and the
previous unit stored none, `CarryOver` refuses rather than silently carrying
everything.

A repeated fact identity is ordinary — two workers, two parts of a subdivided
unit, or the fresh import and the carried predecessor — and its keys
accumulate: the carried set is the union of the keys the fresh import
published and the predecessor's keys that this refresh did not replace. In a
conformant refresh that union is exactly the fresh set, because `Replaced.Keys`
names the complement — every previous key the fresh run no longer emits — so a
fact whose keys changed has its old keys named replaced by the same refresh
that re-publishes it. A producer that changes a fact's keys without naming the
old ones leaves the stale key attached, and a later refresh replacing that
stale key drops a live fact; the contract, not the copy, is what rules that
out. A repeat that describes a node **differently** from the row already
written is refused (`CTX_PROVIDER_OUTPUT_INVALID`, naming the node and the
first differing column): the insert yields to the row that is present, so the
unit would otherwise silently keep whichever description arrived first, and,
since `CarryOver` runs last, a delta-built unit would keep a different one from
a full re-import of the same source. A `relation_facts` row stores nothing but
its identity, so no repeat of one can diverge.

### Aliases

`native_aliases` have neither evidence nor a file column, so they are excluded
by `Replaced.Scopes` — the scope key is opaque to storage, which only resolves
each named scope through `scope_keys` and matches the resulting
`scope_key_id` — and are otherwise carried only while the node they target is a
fact of the new unit, which is exactly the condition `SealUnit` enforces.

## What carry-over guarantees

- **Source agreement.** Carrying a row asserts its source has not changed. The
  call is refused (`CTX_SNAPSHOT_CHANGED`) unless every file the previous unit
  *located facts in*, and that `Replaced` does not name, is declared by the new
  unit with the same content hash. Inputs that carry no facts of their own —
  the index or export the provider read, a build manifest — may change freely.
- **Fresh always wins.** `CarryOver` runs after the provider's own batches and
  before `SealUnit`, and every insert it issues yields to a row already
  present. The same conflict clause is what lets a producer observe one
  declaration from two workers without the unit failing.
- **Evidence is re-identified.** Evidence identity folds the unit id (Section
  9.3), so a copied occurrence is a different row with a different primary key.
  It cannot be copied in SQL; `CarryOver` recomputes `NewEvidenceID` over the
  new unit, joining back out to `node_ids`, `relation_ids` and `native_keys`
  for the canonical values the derivation folds — the row itself holds only
  surrogates. `evidence.content_hash_bound` records whether the producer bound
  the occurrence to its file's content hash, which is the one input to that
  derivation the row would otherwise have lost.
- **The evidence bound is re-applied.** A provider under a delta sees only the
  occurrences it re-emitted and cannot enforce `MaxEvidencePerFact` across the
  rows it did not see. `SealUnit` clips every unit, delta-built or not:
  `NodeFact.Validate` bounds one handed-off batch, which is not the same thing,
  and clipping only the delta path would make the row set depend on how the
  unit was built — precisely the determinism a delta has to preserve. The
  surplus is chosen by evidence id, a function of the occurrence's own content,
  so the retained set is the same for the same union however it was assembled,
  and fresh rows are not preferred to carried ones. `UnitWriter.EvidenceClipped`
  is what keeps that truncation from being silent, and `SealUnit` logs one
  bounded warning with the unit id and the count whenever it is nonzero.
- **A failed delta changes nothing.** `Fail` deletes the new unit's rows. The
  carried rows are the new unit's own copies, so the previous unit is untouched
  and stays sealed and published.

## Delta state

`UnitWriter.PutDeltaState(kind, reader)` stores the artifact the *next*
refresh needs in order to diff without recomputing this one: the SCIP
`DocumentManifest`, the dependence `KeySet`. The payload is opaque to storage,
read from the file it was produced in and stored as a sequence of 1 MiB parts,
so an artifact the size of a large unit is never held whole and has no bound.
It shares the unit's lifetime exactly, so a retired unit takes its manifest
with it and no refresh can diff against state whose facts were collected.
`Store.DeltaState(unit, kind, writer)` streams it back into a file;
`Store.SelectedUnit` finds the predecessor — the unit the previous generation
selected for the same provider and scope.

What a unit *declared* is not delta state and is never stored twice:
`Store.UnitInputs(ctx, unit)` streams a sealed unit's `unit_inputs` rows as an
`iter.Seq2[model.UnitInput, error]` in ascending `FileID` order — the order
`BeginUnit` required when they were written — so an applier can merge-join the
predecessor's declared files against the fresh ones without holding either
list. It refuses a unit that is not sealed: a building unit's rows are still
arriving, and a delta diffed against a partial set would name too few replaced
buckets and carry a stale fact. The dependence applier uses it to compute
`Replaced.Files`; the manifest of the same rows it used to store beside the
unit was deleted with it.

## Applier sketch

```go
prev, _ := store.SelectedUnit(ctx, previousGeneration, providerID, scope)
raw, _ := store.DeltaState(ctx, prev, "scip.document_manifest")
os.WriteFile(manifestPath, raw, 0o600)
previous, _ := scip.LoadDocumentManifest(manifestPath)
defer previous.Close()

w, _ := store.BeginUnit(ctx, gen, build, inputs)
rep, _ := p.Import(ctx, req, sink, scip.ImportOptions{Previous: previous})
sink.Flush(ctx)

// Diff is re-runnable, so the delta is measured once and each replaced set is
// streamed by a walk of its own; nothing is collected.
d, _ := rep.Manifest.Diff(previous, nil)
var filesErr, scopesErr error
replaced := sqlite.Replaced{
    Files: func(yield func(model.FileID) bool) {
        filesErr = changedDocuments(rep.Manifest, previous, func(path string) error {
            if !yield(model.NewFileID(repo, path)) {
                return errStopDocs
            }
            return nil
        })
    },
    Scopes: func(yield func(string) bool) {
        scopesErr = changedDocuments(rep.Manifest, previous, func(path string) error {
            if !yield("file:" + path) {
                return errStopDocs
            }
            return nil
        })
    },
}
stats, err := w.CarryOver(ctx, prev, replaced)
// The stream errors first: a stream that failed mid-walk staged fewer entries
// than the applier named.
_, _, _, _ = stats, filesErr, scopesErr, err

fresh := filepath.Join(workDir, "manifest")
rep.Manifest.Save(fresh)
freshBytes, _ := os.ReadFile(fresh)
w.PutDeltaState(ctx, "scip.document_manifest", freshBytes)
store.SealUnit(ctx, w)
rep.Manifest.Close()
```

This is the sequence the real-tool proof runs (scip-go 0.2.7 over two
copies of this repository, one file edited): `changed=1`, `unchanged=173`, and
the delta-built unit is row-identical to a full re-import across every fact
table.

## Capability details

`generation_capabilities.details_json` stores the bounded diagnostic map a
provider attaches to a capability that is not fresh — which methods a partial
capability skipped, which labels it could not map. Keys are emitted in
ascending order, so the stored text, and therefore the digest folded into the
generation's `AnalysisKey`, is a function of the pairs and never of the order a
publisher added them.

## How an index run commits

Every write an index run makes -- the snapshot's blobs and file rows, each
unit's batches of facts, evidence, aliases and search documents, the seal, the
membership rows, the packed builds and the activation itself -- joins one
write transaction, the **ingestion group**, instead of committing on its own.
Inside the group each call runs in a savepoint, so a refused batch rolls back
alone and the units already in the group are kept. The group commits the
moment the writer's page cache (`storage.writer_cache_kib`, 1 GiB) would
spill a dirty page to the log; when another writer needs the database (a
session, a lease, a heartbeat, a retention sweep: each ends the group before
it runs and never waits longer than one batch); at activation or abort; and
when the store closes. Readers on the reader pool see a run's units at those
commits and not before; the ingestion side reads its own writes (unit states,
aliases of dependency units, the snapshot and blobs a capture recorded) on
the group's own connection, so a run reasons about what it has stored without
waiting for a commit.

The reason is what a commit costs. SQLite writes every page a transaction
dirtied to the log in full, and the checkpoint copies each once more; a batch
of a thousand facts dirties a leaf in every hash-keyed index for almost every
row, plus the root and interior pages of every b-tree it touched. Committing
per batch therefore sent to disk about forty-six pages per commit plus
seven-tenths of a page per fact, measured: the reference repository's index
wrote 47 GB for a 624 MB database, and the store's ingestion test wrote 1 854
MiB for 41 MiB stored. One group per cache-full writes each dirtied page once
per group, and a group whose pages all fit the cache writes nothing at all
until its commit: the same test now writes 73 MiB for 73 MiB stored, by the
engine and to the disk alike, and a run's disk traffic is bounded by the
pages its groups touch, not by the number of batches it commits.

Three engine settings make the group behave that way. The writer's page cache
is the group's bound rather than the log's size, because a group that outgrows
its cache spills dirty pages to the log and rewrites the hot ones there in
place, tens of times each, which the page cache of the kernel hides on a short
run and the disk pays on a long one. The engine's statement journal, which
records the prior image of every page a savepoint touches, stays in memory up
to 64 MiB, one batch's worth, instead of being rewritten to a temporary file
at every batch (fifteen times the bytes stored, measured). And the log is
truncated whenever the engine resets it, so the store can tell a group's first
frame from the log's size and header, and a run never leaves a log the size of
its largest group on disk. The group's commit and the checkpoint that follows
reach the disk as every write of the engine does, through the paced file
system the process registers as its default: after each 8 MiB window
written to a file the writer waits for the previous window to reach the
disk and submits the new one, so at most one window is in flight per file
and the disk receives a commit at its own rate rather than as one burst at
the sync ([ADR-0008](adr/ADR-0008-ingestion-group.md), decision 5). Freeing
is windowed the same way: the file system shim truncates a log or a journal
the engine frees one window at a time, and every file the process removes
itself -- a spool, a sort's runs, a staging tree, an analyzer's export -- goes
through `internal/paced`, which shrinks a file larger than the window one
window per step before it unlinks it, so a filesystem that discards freed
blocks as they are freed never receives gigabytes of discards from one
commit. At activation and abort the
writer's page cache is released to the process, so a long-lived server does
not keep a run's working set resident. The engine's temporary files -- the
spill files of a sort larger than its cache, a statement journal past its
memory threshold, a temporary table too large for memory -- are created
under `<data_dir>/tmp`, which the process names at start-up, so they live on
the disk the user gave the data and never on a memory-backed system temp
directory.
[ADR-0008](adr/ADR-0008-ingestion-group.md) records the measurements, the
alternatives and the residual cost that remains for hash-keyed indexes.

## Durability after a power loss

The database runs in WAL mode and its single writer connection runs at
`synchronous = normal` by default (`storage.synchronous`; set `"full"` to
fsync the write-ahead log after every commit instead). WAL mode at NORMAL is
safe from corruption and always consistent: a power loss or hard reset can
never leave a database that does not open, only one whose most recent commits
have rolled back. Durability across an *application* crash — a panic, a kill,
a failed process — does not depend on this setting at all and is unaffected.

What a rolled-back tail can cost is bounded by two properties of the store.
Generation activation is a single commit, so a generation is either visible or
it is not; a crash can leave the previous generation active, never a partly
published one. And a blob's content reaches the disk before the commit that
names it, so the only inconsistency a rolled-back commit can produce is a blob
that nothing references — an orphan, which the retention collector reclaims: its
CAS sweep walks the store in bounded chunks and removes every object no blobs
row names once it is older than the `retention.blob_grace` window, never a
manifest pointing at content that is not there.

So the worst case is: the workspace is one generation stale and holds some
unreferenced blobs until a sweep a grace window later. Both are repaired by re-running the
index, which the store is built to do cheaply, because everything in it is
derived from the repository's own bytes. `storage.synchronous = "full"` buys
back durability of those last commits at roughly 10 ms of fsync per commit;
[ADR-0004](adr/ADR-0004-wal-synchronous-mode.md) records why that is not the
default.

## The packed per-generation adjacency

A generation publishes, beside its facts, a packed form of its own graph, and
every traversal reads structure from that and from nothing else
([ADR-0005](adr/ADR-0005-graph-traversal-layout.md), Decision 1). It is derived
from facts that are already sealed, so it is neither a fact nor an identity: it
carries no provider version and no analysis fingerprint, and it is dropped with
the generation it describes.

It exists because reading structure from the relation b-tree costs a random
descent per frontier node and a visibility sub-query per candidate edge, and
both costs grow with the repository while the answer does not. Resolving
visibility once, at publication, and scanning a packed array instead turns the
per-page cost into a sequential read.

**The two tables.** `generation_graph` carries one row per generation: the
largest node and relation surrogate, the node and edge counts, and the
relation-kind and node-kind dictionaries. `generation_graph_parts`
carries the bytes, one row per chunk of one stream, keyed by generation, stream
name and a 0-based part number whose parts concatenate to the stream. Both
cascade from `generations`, so collecting a generation collects its graph.

The header row is written **last**, after every part. Its presence is therefore
the commit marker: a reader that finds it is guaranteed every part behind it,
and a half-written graph is indistinguishable from no graph at all.

## The packed lexical segments

The lexical statistics a single-token search term reads are packed too, and the
term reads its inputs from that packed form and from nothing else
([ADR-0007](adr/ADR-0007-lexical-first-page.md), Decision 1). Unlike the
adjacency, the packed form is **not** built at activation: it is a set of
immutable **segments**, each folded once by the seal of the unit whose documents
it holds, and an activation merely names the segments its generation reads.

It exists in packed form because the live posting path costs three b-tree
descents per posting **instance** — the vocabulary row, the document row and the
generation membership probe — plus a temporary b-tree per term for the document
frequency, and all of that is paid again on every request. It is segmented
because a structure rebuilt per generation costs the whole corpus at every
activation, including the activation that publishes one saved file.

**The three tables and the column that ties them together.** `lexical_segments`
carries one row per segment: its term count, its document count and its packed
bytes. `lexical_segment_parts` carries the bytes, one row per chunk of one
stream, keyed by segment, stream name and a 0-based part number whose parts
concatenate to the stream. `generation_segments` is one generation's set in read
order, each row carrying how many of that segment's documents the generation
hides; its reference to a segment is `RESTRICT`, so a segment a generation still
names cannot be collected. Alongside them, `generation_lexical` carries one row
per generation — the visible document count, the total token length and the
visible-document bitmap — and cascades from `generations`.

`search_units.segment_id` is the **source of truth** for both: it names the
segment a document was folded into, it is the document's rather than the row's,
so a carry-over copies it forward with the document rowid, and a compaction
re-points it. A generation's set is derived from it and a segment's garbage
status is decided by it; there is no ownership table.

The three streams of a segment are `term.dir`, a fixed-width directory in term
order holding each term's document frequency and the slices of the other two
streams that belong to it; `term.text`, the concatenated term bytes; and
`post.list`, each term's per-document, column-ascending `(column, count)`
sequence, documents ascending by rowid and encoded as deltas. A term lookup is a
binary search over the fixed-width directory, so a query reads a bounded window
of parts rather than a vocabulary.

`generation_lexical` is written **last**, after every `generation_segments` row,
for the same reason the graph header is: a reader that finds it is guaranteed
the whole segment set behind it.

**Where a segment comes from.** A document is tokenised exactly once, by the
pass that counts its tokens as it is put. That pass stages one row per
`(term, document, column)` group in a staging database of the unit's own, under
the data directory's `tmp` — appended in arrival order, durability off, no index
during the load. At seal, one ordered read of that staging is folded into the
unit's segment, and the staging is deleted; it is also deleted when the unit is
abandoned or failed. That ordered read is the only `ORDER BY` any plan of this
store issues, and it runs over one unit's rows in a database of its own, so no
sorter over the corpus ever exists. A unit that publishes no document folds no
segment.

**What an activation does.** It records the generation's segment set and, when a
tier is full, merges one. The set is **derived**: the distinct segments the live
documents of the generation's member units point at, in segment-id order. A
delta inherits its predecessor's segments through the documents it carried —
each keeps its document rowid and therefore its segment — so inheritance needs
no copy, a segment holding none of this generation's documents is simply never
named, and every visible document lies in exactly one named segment by
construction. Nothing is rewritten to carry a document forward, and a document
whose unit has left the generation is hidden by the generation's
visible-document bitmap rather than removed from the segment that still holds
it.

One pass over the generation's documents produces all three per-document answers
at once: the bitmap, which is stored so no reader scans them again; the
generation's document and token statistics; and how many documents of each named
segment the generation still carries, whose shortfall against what the segment
packed is the hidden count stored per named segment. The pass's start and end are
logged on their own, so an operator can see it rather than infer it from the
activation's total, and it holds the same 5 % share of the index wall clock the
adjacency build is held to.

**Compaction.** A seal folds one segment per unit, so without merging them the
segment count would be the count of units the store has ever sealed and every
term of every query would pay one binary search per segment. Segments are
grouped into size tiers by packed bytes, each tier `r` times as wide as the one
below; while a tier holds more than `r` segments its segments are merged into
one, and a segment more than half of whose documents are dead is rewritten
alone. The merge streams the inputs' parts on (term, document rowid), copies a
term's posting bytes verbatim when only one input carries it and that input has
no dead document, and writes the merged parts through the ingestion group, so
nothing of a segment is ever resident whole. The tier width, the ratio and how
many inputs one merge reads are internal layout constants: they decide how often
already-packed bytes are rewritten, never what may be stored or answered.

A merge is a **soft** deletion. It KEEPS a document the activating generation
merely hides: that document's unit left the generation but can be attached to a
later one at any time, and because the index is contentless its postings are the
only copy of the text it was indexed with. It DROPS only dead documents — rowids
no `search_units` row names any more, because the unit holding them was
collected. It re-points every row of its inputs, the members' and the retired
units' alike, in the same activation, so afterwards no row names an input, a
reattached unit finds its documents where its rows point, and a segment can never
be named beside the one that absorbed it. A generation published earlier keeps
its own rows and reads the inputs until it is collected.

**What a read does.** A term is one binary search per segment, and the segments'
posting streams are merged by document rowid, so the candidate walk still
receives one strictly ascending sequence. Every visible document of a generation
lies in exactly one of its segments; a document two segments both claim is
reported as a corrupt store rather than delivered — and scored — twice. The
document frequency is exact: a segment the generation hides no document of
answers from its directory entry, and one that hides some is counted by walking
the posting list against the bitmap,
because a frequency that counted hidden documents would make a ranking depend on
the store's history instead of on the code.

A segment that no retained generation names and that no document points at is
deleted in the same transaction as the generation or the unit that released it —
which is exact because both tests are read off the documents themselves, so a
segment every one of whose documents a carry chain has dropped, and one a merge
has emptied by re-pointing its rows, both stop being reachable. A build that died
before its seal is collected the same way, and its staging database is removed
with it; the `tmp` directory is never swept on its own, because several processes
may share one data directory and a live build's staging is indistinguishable from
a dead one's.

A **phrase** keeps the live posting path. The packed form stores counts, not
offsets, and a phrase has to test adjacency; storing offsets would put a second
copy of the indexed text back in the database, which is exactly what the
contentless index above exists to avoid.

**The streams.** Per direction there are two: an offsets directory of one 64-bit
little-endian entry per node surrogate, and an edge stream. A node's list starts
at its own offset and ends at the next node's, so a node with no edges has two
equal offsets; one extra entry past the largest surrogate holds the stream
length, which removes the last node's special case. Each list is sorted by
neighbour surrogate and encoded as a variable-length delta of the neighbour, a
variable-length relation surrogate and a one-byte relation-kind code.

Four fixed-width arrays sit beside them, indexed the same way: each node's kind
code, its container surrogate, and the source size of a file node, and each
relation's evidence count. Zero is "absent" in all four, so a surrogate the
generation does not carry costs one zeroed entry and never a lookup. The
container is the node itself when the node is a container kind; otherwise it is
the container-kind node that claims the node's **file**, overridden wherever a
container-kind node claims the node **directly** by `contains`. Either claim
settles ties on the **lowest canonical id** — the 32-byte identity, never the
surrogate, so the attribution is a fact of the ids and not of the order the
repository happened to be indexed in. A node whose file publishes no container
and that nothing contains keeps a zero slot: neither a directory nor a file
answers "which package does this symbol belong to".

Parts are a fixed size except the last of each stream, which makes locating a
byte pure arithmetic. A list may straddle a boundary and the reader stitches it.
The part sizes and the reader's window are internal layout constants, not
settings: a larger repository yields more parts, and nothing is ever refused,
skipped or truncated because of them.

**The build** runs inside activation, before the active pointer flips and in the
same transaction, so a generation is never published without the structure its
readers expect and a failed build fails the activation. It first materialises
the generation's visible relation and node surrogates, then streams two ordered
scans of the relation dictionary, one per direction, emitting each offset as the
scan passes its node and flushing each part as it fills. The scans run in the
index order the dictionary already provides and each node's list is re-sorted by
neighbour in memory, one list at a time, so neither scan sorts the relation set
and the working set is one part plus one list rather than the graph. The side
arrays come from one ordered pass over the generation's node facts, merged
against one ordered pass of the container claims, and one aggregate over
evidence. The build logs its own start and end, so its share of an indexing run
can be read off without inferring it from the total.

