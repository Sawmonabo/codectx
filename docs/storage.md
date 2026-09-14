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
| `internal/provider/scip` | one SCIP document | `Replaced.Files` (the changed and removed paths' `FileID`s) and `Replaced.Scopes` (their alias scopes) |
| `internal/provider/dependence/neo4jcsv` | one fact | `Replaced.Keys` (the changed and removed fact keys) |

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

### The fact key

`node_facts.fact_key` and `relation_facts.fact_key` hold the producer's own
id-independent key, supplied through `PutKeyedNodes` / `PutKeyedRelations`
(`provider.DeltaSink`). Storage never derives or interprets it.

A key is needed because a path bucket cannot express every removal. A
dependence edge can disappear while every file holding its evidence is
unchanged — the edit was in the *callee's* file, the evidence is in the
caller's — so only the producer can say the fact is gone. It cannot say so by
fact identity either: a `RelationID` is derived from the resolved endpoints,
which an edit changes, so the removed fact has no identity to name.

Rows written with no key (`fact_key = ''`) are never key-excluded; only their
bucket can replace them. If the applier names replaced keys and the previous
unit stored none, `CarryOver` refuses rather than silently carrying everything.

A row holds **one** key, so a producer's keys must be one per published fact
identity, not one per emitted row. Several rows collapsing to one identity is
ordinary — it is why `provider.DedupeSink` exists — but if they arrived under
*different* keys the stored row would remember one of them, and a refresh that
removed only that key while the others still held would drop a fact the source
still contains, with nothing to re-emit it. `PutKeyedNodes` and
`PutKeyedRelations` refuse the second key
(`CTX_PROVIDER_OUTPUT_INVALID`) rather than store a row that can be lost later.

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
  is what keeps that truncation from being silent.
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
prev, _ := store.SelectedUnit(ctx, previousGeneration, scip.ProviderID, scope)
manifest, _ := store.DeltaState(ctx, prev, "scip.document_manifest")
// ... write manifest to a file, scip.LoadDocumentManifest, Import with it ...
w, _ := store.BeginUnit(ctx, gen, build, inputs)
rep, _ := p.Import(ctx, req, sink, scip.ImportOptions{Previous: previous})
var replaced sqlite.Replaced
rep.Manifest.Diff(previous, func(c scip.Change) error {
    if c.Class != scip.ClassUnchanged {
        replaced.Files = append(replaced.Files, model.NewFileID(repo, c.Path))
        replaced.Scopes = append(replaced.Scopes, "file:"+c.Path)
    }
    return nil
})
stats, _ := w.CarryOver(ctx, prev, replaced)
w.PutDeltaState(ctx, "scip.document_manifest", freshManifestBytes)
store.SealUnit(ctx, w)
```

## Capability details

`generation_capabilities.details_json` stores the bounded diagnostic map a
provider attaches to a capability that is not fresh — which methods a partial
capability skipped, which labels it could not map. Keys are emitted in
ascending order, so the stored text, and therefore the digest folded into the
generation's `AnalysisKey`, is a function of the pairs and never of the order a
publisher added them.
