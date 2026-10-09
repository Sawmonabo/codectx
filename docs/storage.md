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
`CHECK(length(canonical_key) = 32)` — 32 bytes, not 64-character hex text, and
unique only in combination, through `UNIQUE(kind, canonical_key)` — and neither identity table carries a
`repository_id` column, because one store is one repository.
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
resolved or validated against the snapshot. A search document is not re-derived
either. The source over a document's byte range is not the body its producer
published — the tree-sitter provider publishes a symbol's names, signature and
attached documentation over the whole declaration's extent, and the filesystem
provider publishes no body — so rebuilding a document from the file would index
text that was never any document's body and silently change that document's
hits, BM25F length and snippets on every delta. The carry-over copies the
documents' identity rows and the unit's document and token totals, and the
documents themselves stay in the search index exactly as they were added. The
whole carry-over reads and writes no text.

## The search index

The lexical tier's postings live in an embedded search library, built in this
repository as a static library and linked into the one binary
([ADR-0013](adr/ADR-0013-search-index.md)). Its index is a directory of its own
under the data directory: compressed postings with frequencies and positions,
immutable segments read through memory maps, a dictionary per column, and fast
(columnar) fields for `unit`, `kind`, the path and `token_count`. SQLite keeps
what is not postings:

- each search document's identity row in `search_units`, from search key to
  node, file and byte range;
- each unit's document count and total token count, so a generation's N and
  total token length are sums over its visible units;
- everything that is not search: files, symbols, relations, evidence, snapshots
  and units.

No text is stored twice. The search index holds what a hit serves; the source
itself lives once, in the content store, and every answer that needs source
bytes — the hydrated window around a hit — reads them there through the
verified range reader, over the `file_id` and byte range the identity row
carries. Generic results still carry no body (Section 14.2).

**One index holds every retained unit.** The library keeps only its latest
commit, so a snapshot is not an index directory. A unit's documents are added
once, when the unit is first built, carrying the unit's content-keyed digest in
the `unit` field. A generation is a **visibility set** over the one index: its
member units, read from SQLite, resolved once per term ordinal of the `unit`
fast field and expanded per segment into a bitset over document ids. Hidden
documents are never candidates, and they never count in N, in the total token
count or in a term's document frequency, so a generation ranks exactly like an
index of only its visible documents. Which route applies that bitset — each
segment's alive set, a query wrapper, or the collector — is **pending**: it is
chosen by its time at 10% and 50% of units hidden on the reference export, and
all three give identical pages.

**Writing and committing.** The library's multi-threaded writer takes its
thread count and memory budget from the admission's measured need, like every
other worker; its merge threads and the search executor are governed the same
way, and none of them has a fixed constant. A unit's documents are added in one
call, and the index commits **once per generation activation**, never once per
unit: a commit costs one fsync, and a commit per unit would cost minutes on a
large repository. Units added between commits are invisible until their
generation activates. The library merges its segments in the background, so an
activation never touches postings.

**Crash order and recovery.** The library commits first, with the generation's
id as its payload, and SQLite activates the generation after that. A
generation SQLite has activated is therefore always committed in the index. On
start, reconciliation deletes the documents of units that no SQLite generation
references and re-plans any unit whose documents never reached a commit, like
any other unsealed unit. A unit's documents are deleted only when collection
frees the unit, by term on the `unit` field. Nothing in the index directory is
a source of truth: an index lost with its data directory is rebuilt by
re-indexing, as the rest of the directory is.

## Discarding a unit: two costs, two paths

`UnitWriter.Fail` unwinds a building unit synchronously: every
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
| `internal/provider/dependence/graphcsv` | one fact | `Replaced.Keys` (the changed and removed fact keys) — shipped: `emitNodes`/`emitRelations` hand every key behind each fact to `PutKeyedNodes`/`PutKeyedRelations` (`fact_keys` holds one row per key), and `KeySet.Diff` yields the replaced set; the applier that passes it to `CarryOver` is `internal/index/delta` — shipped. |

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
(`read_connections`; by default one per unit built at once plus one per
query slot) once per page of its own scan while the write
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
`Replaced.Files`; no manifest of the same rows is stored beside the
unit.

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

## A second process reading while a run writes

A process that only answers questions opens the store with **no writer
connection at all**. Nothing it does can wait on the ingestion group another
process holds, and nothing it does can delay that group: the reader pool is
`query_only`, it reads a write-ahead log snapshot, and a log reader never waits
on a writer.

That is not the default it looks like. A read-only command performed two
writes before it read anything, and both were `_txlock=immediate` on the single
writer connection, so both queued behind a run's group and were refused
`database is busy` after `storage.busy_timeout`:

* **the schema check at open**, which ran the fingerprint comparison inside a
  write transaction because the same call creates the schema in an empty cache;
* **the retention lease** every pinned generation inserted.

Opened without a writer, the fingerprint is compared on the reader pool -- the
same comparison, and a cache with no tables is reported as the workspace that
was never built rather than as a fingerprint that failed to match -- and the
pin takes no lease.

**A pin without a lease is a snapshot, and a snapshot is what a query needs.**
Every read such a reader serves runs in one deferred read transaction of its
own, so within a call the log snapshot is fixed: a collection running in another
process at that moment cannot take rows out from under the read, and the answer
in hand is the whole answer for the generation it was bound to. The pin
therefore accepts any published generation, superseded as readily as active; the
lease was retaining rows against a hazard the snapshot already excludes.

What the snapshot does not span is the gap BETWEEN calls, and that gap has an
answer rather than a hole. A continuation names the generation its token was
minted against, and both continuable endpoints pin that generation rather than
the current one. If the retention pass behind another process's activation has
collected it, the pin finds no row, and the caller is told so in the family a
continuation is refused in -- `CTX_CURSOR_INVALID`, re-run from the first page
-- and not as though the argument it never typed were malformed. A generation
named by hand with `--generation` still meets the argument refusal, which is the
answer that case asks for.

Every mutating entry point on such a store refuses with `CTX_INTERNAL`: it is
reachable only by composing a command that changes the workspace with the
composition that cannot, which is a defect in this binary and not an operator
state.

## How an index run commits

Every database write an index run makes -- the snapshot's blobs and file rows,
each unit's batches of facts, evidence, aliases and search identity rows, the
seal, the membership rows, the packed adjacency build and the activation itself
-- joins one write transaction, the **ingestion group**, instead of committing on
its own. A unit's search documents go to the search index's writer instead, and
reach a commit once per activation, before SQLite activates the generation
([the search index](#the-search-index)).
Inside the group each call runs in a savepoint, so a refused batch rolls back
alone and the units already in the group are kept. The group commits the
moment the writer's page cache (`storage.writer_cache_kib`, 1 GiB) would
spill a dirty page to the log; when another writer needs the database (a
session, a lease, a heartbeat, a retention sweep: each ends the group before
it runs and never waits longer than one batch); at activation or abort; and
when the store closes. Readers on the reader pool see a run's units at those
commits and not before. The index build commits three more times on purpose:
after the capture, before the build, and at each provider boundary. So the
reads a unit makes -- its dependencies' states and aliases, its inputs, and
every manifest row and blob it reads through the snapshot view -- run on the
reader pool against the last commit and never queue behind the writer. The
ingestion side reads its own writes on the group's connection only where it
reasons about what this run has stored since the last commit: the capture's
check for a blob it already recorded, the selected and carried units, delta
state, generation status and provider runs.

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
at every batch (fifteen times the bytes stored, measured); when it does spill,
the shim hands its chunks to the file system in pieces the engine's own write
path accepts whole. And the log is rewound in
place rather than truncated when the engine resets it: no journal size limit is
set, so the file keeps its high-water length -- bounded by the log the largest
group leaves -- and every later group writes over the space it already holds. A
writer that truncated its log at every reset would hand the filesystem the whole
log's blocks several times in one run, in the middle of its work. Because a
rewind writes inside the file the log already holds, its size no longer says
whether the writer's page cache has begun spilling; the file system shim sees
every write the engine makes, and the engine names a log in the open flags, so
the shim counts the bytes written to a log and the store marks that count when a
group begins. The group's commit and the checkpoint that follows
reach the disk as every write of the engine does, through the paced file
system the process registers as its default: after each 8 MiB window
written to a file the writer waits for the previous window to reach the
disk and submits the new one, so at most one window is in flight per file
and the disk receives a commit at its own rate rather than as one burst at
the sync ([ADR-0008](adr/ADR-0008-ingestion-group.md), decision 5). Freeing is the other half, and it is not the same problem. Windowing bounds
what the machine submits; it does not bound what its host owes. Where the
filesystem discards freed blocks and the machine's disk is a sparse image, a
free of several gigabytes becomes host-side work the machine can neither
observe nor wait for, and about a minute later some request in flight waits a
minute for it. Measured both ways: 5 GB freed in one burst hung the disk for
64 s; the same 5 GB freed as 8 MiB windows, each followed by a data sync and a
250 ms wait, hung nothing ([ADR-0008](adr/ADR-0008-ingestion-group.md),
decision 5).

So the process frees nothing where it is asked to. A removal **renames** its
file or tree into a to-free set -- a rename frees nothing and costs a directory
entry -- and returns; the caller goes on. **One reclaimer per process** gives
the space back on its own goroutine, a window at a time, each window followed
by a data sync and that wait, for as long as the process runs. The pace is a
constant of `internal/paced`, not a setting: it is a measurement of this class
of host, and there is nothing for an operator to tune. Bytes are charged to one
budget whichever way they were released, so a burst of whole-file unlinks of
small files -- a materialized tree of thousands of source files -- is given
back at the same rate as one large file, and a caller that frees in place
spends the same windows as the reclaimer.

The rate is the **host's**, because what the disk underneath does with a
discard does not depend on how many callers asked. Every charger in a process
waits under one lock, so the reclaimer's goroutine, the file system shim
shortening a file the engine owns and a publication trimming its staging
surface never hand the disk three windows at once. Across processes the turn
is taken through one small lock file per user on the host, in the product's
directory in the user cache, above every data directory: a window's turn is
locking that file, waiting out whatever remains of the interval since the last
recorded turn, recording this one and unlocking, so an index run and a query
server free at the pace between them rather than at twice it, whichever data
directories they are writing into, because the rate belongs to the device and
not to a directory.
The remainder is clamped into one interval, because the recorded time is a
wall clock written by another process and a clock adjustment must cost at most
one interval rather than hang a run. A process with no user cache directory
takes its turns through the outermost data directory it has registered, and
so shares the pace only with the processes over that directory. One that can
reach neither -- a standalone tool on a read-only home -- keeps the pace for
itself alone: the same rate while it is the only one, and twice it if a second
joins, which is why that path is a last resort.

A file the process may unlink but may not truncate -- every published blob is
one, the store making its objects read-only at publication, and nothing bounds
a blob's size -- reaches the unlink whole, so the unlink is what gives its
whole length back in one act. Those bytes are therefore charged **before** the
unlink: the removal waits its own size's worth of windows and the unlink is
the last thing that happens, rather than the first. The directory is asked
whether it permits the unlink before the wait: an unlink it refuses is
reported at once and charges nothing, so an entry stuck behind a refusing
directory neither repeats its whole length of waits at every retry nor holds
the entries queued behind it. A file another name still
reaches owes nothing either way, because unlinking one of an object's names
gives no blocks back; it is neither paced nor counted until the last name
goes.

The to-free sets are on the disk, inside the pool instance the process claims,
so a run that exits or crashes with removals queued leaves them for the next
process to take over and finish at the same pace. Nothing is ever freed faster
because it is old.

Three figures in the resources block follow from this. `freed_bytes` is what
the process has actually given back, counted byte by byte as each truncation
and each unlink released it. `freed_by_purpose` is the labelled part of that
same counter, split by what the removal was for, so the two cannot disagree.
`pending_free_bytes` is what has been removed and not yet released: between the
rename and the release the space belongs to neither the pool nor the freed
total, and reporting only the other two would leave it invisible.

A queued removal the filesystem refuses -- a device error, a read-only mount, a
directory a failed child left without write permission -- is passed over rather
than retried, so the removals queued behind it are still given back, and it is
retried the next time something is queued. It is named, with the reason, in
`stuck_frees` beside `pending_free_bytes`: that figure climbing and never
falling is either a run removing faster than the pace gives back, which
resolves itself, or a removal nothing can make, which does not, and only the
list beside it tells the two apart. A name stands until its removal is made or
its entry is gone. A to-free directory that cannot be listed is named the same
way until a listing succeeds, and one that is no longer on the disk holds
nothing and is dropped, never named. The request that empties the pools reports
the same list rather than waiting for a removal that will never succeed. Both
carry one page of it, at the bound every record list of a response carries,
and `stuck_frees_omitted` counts the entries past that page.

At activation and abort the writer's page cache is released to the process, so
a long-lived server does not keep a run's working set resident.

The engine's own temporary files -- the spill files of a sort larger than its
cache, a statement journal past its memory threshold, a temporary table too
large for memory -- are **pooled** as well. The engine asks its file system for
an unnamed file to be unlinked on close; the shim answers with a surface from
the pool and the close gives it back holding its bytes, so a sort neither
creates nor frees a file of the size it just spilled. The engine sees an empty
file: the shim keeps that tenant's own length for the surface and answers
reads, the file size and truncations from it, so a read past what this sort
wrote is the short read a file system gives at the end of a file rather than
the previous sort's records. A write PAST that length -- a temporary
database's pager writes each page where the page belongs, not in order --
zeroes what it skipped, so a read below the length and above what this tenant
wrote is the hole a file system would give rather than the previous sort's
records. Those zeroes are real writes and a gap is as wide as the offset the
pager jumped to, so the fill waits a window at a time like every other write
through the shim rather than handing the disk the whole gap at once. The engine shortening its temporary back to zero is a reset of that
length rather than a free.

The pool is the one of the engine's temporary directory, which the process
names once, at start-up, before any store is opened: `<data_dir>/tmp` for a
command that builds, so a run's temporaries live on the disk the user gave the
data and never on a memory-backed system temp directory; for a command that
only answers, the same directory when a run made it and it can be written, and
otherwise a directory of the command's own in the process temp directory,
given back when it closes ([operations](operations.md)). The pool is named only
after the engine has accepted the directory, which it does only for one it can
write, and the first directory named holds the pool for the rest of the
process: named before that check, a directory on read-only media would keep
the pool while the command fell back to another, and every temporary would be
taken from the one directory that cannot hold it.

### The scratch pool

Freeing is the expensive act, so the working files a store writes for its own
later reading are not freed at all. They are taken from a **scratch pool** under
the directory they belong to and given back to it: an external sort's runs and
its merged output, the surface a blob is
streamed into while it is hashed, a shortest-path search's state, an index
import's symbol spool, a dependence import's staging database, and the
engine's own temporaries. `Take` hands out an existing file of the asked-for
purpose, opened for overwrite at offset zero, or creates one; `Release` returns
it for the next taker. **Nothing truncates a surface to reuse it**: a file
keeps whatever length its largest tenant gave it, and re-indexing an unchanged
repository or answering a second query of the same shape therefore frees
nothing at all. The one truncation in the pool is the content store's, which
trims a staged blob to the length its record states *before* the object is
published -- the published object is that file, and a reader proves an object
by its length ([snapshots.md](snapshots.md#the-content-addressed-store)) -- and
that trim is windowed and charged to the same budget as every other free.

A surface the pool cannot open is not handed out. `Take` opens the file it is
about to lease, and one it cannot open leaves the pool *and* the disk: the pool
is a stack, so a surface that had become unopenable would otherwise be handed
to every later taker of its purpose, and be found again by the sweep after a
restart. A tenant that finds the surface's own contents unusable -- an image a
crash left mid-write, whose tables cannot be dropped -- retires it the same
way.

Because nothing truncates, **the file's length is not the data's length**. Every
tenant carries its own logical length — a header, a record count, a byte count
it recorded when it wrote — and never learns it from the file: a sort run is
read through a limited reader over the byte count it recorded, a staged blob is
trimmed to the length its record states before it is published, and a database
surface is emptied by dropping its tables, which returns their pages to the
database's own free list without giving the filesystem anything back.

A pool is per process. It lives under a numbered instance directory the process
claims with an exclusive lock held for as long as it runs, so no two live
processes ever share a surface — several readers serve pages from one data
directory at once, and a search's state file is not a thing two of them may
each hold. A process that exits leaves its instance unlocked and the next
process claims it and takes over its files: an abandoned pool is **inherited**,
never accumulated and never swept.

The disk the pools hold is reported in the resources block as `scratch_bytes`,
and it is the other half of `freed_bytes`: what would have been freed and
re-created over and over is space the store is holding instead. It shrinks only
when an operator asks for the pools to be emptied, with **`codectx gc`** —
there is no timer, no size threshold and no configuration key, because reuse
has no knob and a threshold would be this product choosing, on the operator's
behalf, a moment to stall their host. `gc` prints what each pool holds by
purpose before it empties it, says that freeing a large amount of space can
stall some virtual hosts for about a minute, and drains through the same
reclaimer at the same pace. Emptying skips any instance a live process still
holds, and reports what it did not empty for that reason rather than reporting
a smaller figure.

### What a run still frees

Everything below is given back a window at a time, with a data sync and a wait
between windows, charged to one budget and counted in `freed_bytes`. The two
halves differ in who waits. A **removal** is renamed aside and freed by the
reclaimer, off the run's path. An **in-place** free is a truncation of a file
the caller keeps, or an unlink the caller must have completed when it returns,
so it happens where it is asked for and the caller waits the windows it spends,
at the same rate as the reclaimer's own and through the same turn.

| What | How | Why it is not pooled | Purpose |
| --- | --- | --- | --- |
| An indexer's index file, a dependence engine's export directory | Removal | A foreign writer chose its layout and its length, so it cannot be written over | `analyzer-output` |
| A materialized source tree | Removal | A unit materializes only its own files and an analyzer writes into the tree it was given, so one tree cannot be handed to the next unit | `materialization` |
| A published blob a retention sweep collected, and an orphan the sweep found | Removal | The object is content the store no longer names | — |
| The trim of a staged blob before it is published | In place | The published object is that file and a reader proves it by its length, so it must be that length before the publication, not after | — |
| The staging name of a blob that has just been published | In place, freeing nothing | Two names of one object; the object stays and the unlink gives no blocks back | — |
| A walk's retained level files and its retained directory | Removal | The existence of a level's admitted file is that level's commit record, so a pooled file that always exists could not carry it | — |
| A retained level cut back to the byte count a resumed cursor carried | In place | The file goes on being appended to, so there is nothing to rename away | — |
| A walk's frontier set cleared at a level transition | In place | The set is rebuilt into the same file for the next level | — |
| A continuation's spool and a retained search directory a lease reclaimed | Removal | State a run left for a caller that never came back | `lease-reclamation` |
| Sort runs a sort adopted from a continuation | Removal | They are that continuation's files, not the pool's | — |
| A capture's staging database | Removal | It belongs to exactly one capture | — |
| A dependence import's key set, emptied before the next is written over it | In place | The file keeps its name across imports | — |
| A tool payload reset before a download is retried | In place | The staging file is written again from the start | — |
| The engine's journals and its log, larger than one window | Removal | The engine deletes the log from inside its close, holding the database exclusively for the whole call; emptying it there is a quarter second of held lock per window, which every other process waits out at its next open | — |
| The engine's journals and its log, one window or smaller | In place | A rename would cost a directory entry and a turn of the pace to give back less than one window | — |
| The pools themselves, on `codectx gc` | Removal | An operator asked | `scratch-collection` |

The list is enforced, not narrated: a test walks the source tree and fails on
any call outside the pacing package that gives a file's blocks back and is
neither routed through it nor written down beside it with its reason. That is
every `os.Remove`, `os.RemoveAll`, `os.Truncate` and file `Truncate`, and also
every TRUNCATING OPEN -- `os.Create`, and `os.OpenFile` with `O_TRUNC`, the
flags read through any local variable that carries them -- which frees an
existing file's blocks just as a truncation does.

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

## The run ledger beside the store

A workspace holds a second database, `ledger.db`, beside `codectx.db` in the
same directory and under the same directory lock. It is what a run records
about itself: how long each stage took, what it cost and what went through it
([diagnostics](diagnostics.md#the-run-ledger),
[ADR-0011](adr/ADR-0011-run-ledger.md)). It holds `runs`, one row per run, and
`spans`, one row per stage, unit or part of a unit, keyed to its run and to
its parent span. A third table holds a single row: the ledger's own schema
fingerprint, which is that file's alone and never mixed with the store's.

### Why it is not a table in the store

Two properties of this store, both of them decisions taken for good reasons,
make a table inside it useless for the thing the ledger exists to do.

The ingestion group holds one transaction open for most of a run and commits
when its page cache would spill or an exclusive writer waits
([ADR-0008](adr/ADR-0008-ingestion-group.md)). Rows written through it are
invisible to a second process until that commit, so a ledger inside it could
not answer a question about a run that is still going -- which is half of what
it is for.

Activation holds an exclusive transaction around the adjacency build. Any other writer on that file waits or fails `busy` for its
duration, which is exactly the window an operator most wants an answer in.

There is a third reason that has nothing to do with contention: the store's
schema text is hashed into a fingerprint that is folded into every analysis
key, so adding a table there would re-key every analysis unit in the product
and invalidate every cache. A diagnostic table is not worth that.

### One writer, any number of readers

The ledger has exactly one writer -- the collector goroutine of the process
that holds the workspace lock -- and any number of read-only readers at any
moment. The collector lives exactly as long as that hold: it is opened when a
process takes the workspace lock to build and stopped, with its last rows
flushed, before the lock is given back. A session that holds nothing writes
nothing here, so a server left connected between refreshes is never a second
collector beside the person's own run. It runs in WAL mode at `synchronous = normal`; a reader opens it with
`query_only` set, so `status --resources` reads a run while that run writes,
disturbs nothing, and cannot create the file in a workspace where no run has
ever been recorded. A composition that only reports opens no ledger at all.

### How a run says it is still alive

A run row states a deadline its writer undertakes to renew while it lives, and
the collector renews it on the flush it already performs. That is the only way
a second process can tell a run still being written from one whose process
died: a row that says `running` says nothing about whether anyone is still
writing it, and probing a process id is neither portable nor free of races.
The store already answers the same question the same way for the watch
heartbeat, and a second mechanism for it would be one too many.

A run whose deadline has elapsed reads `interrupted`, and so do the spans it
left open. It is not given a finish time and not given a wall: nobody measured
when it ended.

### Retention

A run's rows follow the generation they describe. When retention sweeps a
generation out of the store, the same pass deletes that generation's runs from
the ledger, and each run's spans go with it by cascade.

Two kinds of run no generation will ever claim, so a second sweep reaches
them, in the collection pass beside the store's own: the `overlay` run a
process opens for work that belongs to no generation, such as starting a
language server; and a run that ended without publishing anything, which the
deferred publication produces on every tick that finds nothing to publish.
Both sweeps skip any run whose writer is still renewing its deadline, so the
pass can never delete a run another process is writing or one a status surface
has just reported live. A process that exits cleanly forgets its own overlay
run before it can be written, rather than deleting a row that its own
collector might still insert behind the delete.

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

