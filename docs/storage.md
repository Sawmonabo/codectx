# Storage: delta imports

`internal/storage/sqlite` is the Section 12 store. This document covers the
one thing the DDL of Section 12.2 does not explain on its own: how a provider
that can tell what changed since its last run seals a new unit as "the
previous unit plus a delta" (Section 11.4) without weakening any of the
guarantees a sealed unit carries.

Everything else about the store — the single writer, the current-schema-init
fingerprint, generation activation, retention by distinct ref — is Sections
12.2–12.4 of `docs/implementation-plan.md` and is not restated here.

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

The copy is also strictly cheaper than the re-import it replaces: nothing is
decoded, resolved, validated against the snapshot or re-tokenized —
`search_units.token_count` is copied, not recomputed.

## The two granularities

Two wave-A importers produce deltas at two different granularities, and
`Replaced` expresses both.

| Producer | Delta unit | What the applier names |
|---|---|---|
| `internal/provider/scip` | one SCIP document | `Replaced.Files` (the changed and removed paths' `FileID`s) and `Replaced.Scopes` (their alias scopes) — **shipped**; the applier sketch below is against the code as it stands |
| `internal/provider/dependence/neo4jcsv` | one fact | `Replaced.Keys` (the changed and removed fact keys) — shipped: `emitNodes`/`emitRelations` hand every key behind each fact to `PutKeyedNodes`/`PutKeyedRelations` (`fact_keys` holds one row per key), and `KeySet.Diff` yields the replaced set; the applier that passes it to `CarryOver` is Task 12's. |

`Replaced` names the **complement** — what does *not* survive — rather than the
survivors. For both importers that is a handful of entries against tens of
thousands: a one-file edit of this repository replaces 1 of 154 SCIP documents,
and an unchanged engine export replaces 0 of 77,701 fact keys.

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
is every key backing `facts[i]`, at least one, each a lowercase hex digest,
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
by `Replaced.Scopes` — the scope key is opaque to storage, which only matches
the column — and are otherwise carried only while the node they target is a
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
  new unit. `evidence.content_hash_bound` records whether the producer bound
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

`UnitWriter.PutDeltaState(kind, payload)` stores the artifact the *next*
refresh needs in order to diff without recomputing this one: the SCIP
`DocumentManifest`, the dependence `KeySet`. The payload is opaque to storage
and bounded by `MaxDeltaStateBytes`. It shares the unit's lifetime exactly, so
a retired unit takes its manifest with it and no refresh can diff against state
whose facts were collected. `Store.DeltaState` reads it back;
`Store.SelectedUnit` finds the predecessor — the unit the previous generation
selected for the same provider and scope.

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

var replaced sqlite.Replaced
rep.Manifest.Diff(previous, func(c scip.Change) error {
    if c.Class != scip.ClassUnchanged {
        replaced.Files = append(replaced.Files, model.NewFileID(repo, c.Path))
        replaced.Scopes = append(replaced.Scopes, "file:"+c.Path)
    }
    return nil
})
stats, _ := w.CarryOver(ctx, prev, replaced)

fresh := filepath.Join(workDir, "manifest")
rep.Manifest.Save(fresh)
freshBytes, _ := os.ReadFile(fresh)
w.PutDeltaState(ctx, "scip.document_manifest", freshBytes)
store.SealUnit(ctx, w)
rep.Manifest.Close()
```

This is the sequence the lane's real-tool proof runs (scip-go 0.2.7 over two
copies of this repository, one file edited): `changed=1`, `unchanged=159`, and
the delta-built unit is row-identical to a full re-import across every fact
table.

## Capability details

`generation_capabilities.details_json` stores the bounded diagnostic map a
provider attaches to a capability that is not fresh — which methods a partial
capability skipped, which labels it could not map. Keys are emitted in
ascending order, so the stored text, and therefore the digest folded into the
generation's `AnalysisKey`, is a function of the pairs and never of the order a
publisher added them.
