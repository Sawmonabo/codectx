# Providers, the sink and identity resolution

`internal/provider` is the provider runtime of Section 11.1: the contract every
fact producer implements, the registry that validates and orders providers,
the byte- and record-bounded sink their output flows through, and the one
place a unit is sealed or discarded. `internal/reconcile` is the deterministic
identity resolution of Section 9.4. This document is the contributor-facing
description of how a provider is written and what it may rely on.

## What a provider is and is not

A provider emits **small immutable units with explicit inputs**. It never
holds a mutable global graph, never writes SQL, never reads the live checkout
and never resolves identity on its own. The coordinator (`internal/index`,
Task 12) decides which units exist, opens each one in storage with its exact
inputs and dependencies, and hands the provider a `UnitRequest`; the provider
produces exactly that unit through the `Sink` and reports a `ProviderResult`.

External tools (SCIP indexers, Joern, language servers) run through
`internal/process`. There is no second process abstraction and no plugin
loader: providers are Go packages composed into one `Registry` by
`internal/app`.

```go
type Provider interface {
    Descriptor() model.ProviderDescriptor
    Detect(context.Context, workspace.Root, workspace.Policy) (Detection, error)
    IndexUnit(context.Context, UnitRequest, Sink) (model.ProviderResult, error)
}
```

`Detect` receives the confined workspace root and the traversal policy rather
than the specification sketch's `workspace.View`, which does not exist: `Root`
is the only safe way to open a repository file and `Policy` is what decides
eligibility. Detection inspects declared inputs (a `go.mod`, an installed
tool) and reports a bounded `Detection`; it must not walk the repository into
memory. An unavailable result carries a Section 22 diagnostic code
(`CTX_TRUST_REQUIRED` for an unapproved executable, `CTX_PROVIDER_UNAVAILABLE`
for an absent one) and is honest absence, distinct from a failure.

`UnitRequest` carries the staging `Binding`, the immutable `UnitSpec` storage
opened, the pinned `SnapshotView` every byte must be read from, the `Resolver`
for this unit's dependencies and the producing `Run`. `Run` is not in the
sketch; it is required because every `Evidence` row names its origin run and
storage rejects evidence for any other run.

## Registry: validation, order and selection

`NewRegistry(providers...)` validates every descriptor and the graph they form
in one step. A duplicate ID, a dependency on an unregistered provider or a
dependency cycle is rejected (`CTX_ARGUMENT_INVALID`): a coordinator scheduling
such a graph could report units as ready whose dependencies never complete.
`Providers()` returns the stable dependency order — Kahn's algorithm with a
sorted ready set, so the order is a function of the descriptors alone.

`Select` runs detection in that order and separates the outcomes Section 11.1
and 13.3 require to stay apart:

| Configured state | Detect outcome | Category |
|---|---|---|
| `false` | not run | `unavailable`, `CTX_PROVIDER_UNAVAILABLE` |
| `auto` | `Available: false` | `unavailable`, the detection's diagnostic code |
| `auto` | error | `failed`, the error's code |
| `true` | `Available: false` | `failed`, the detection's diagnostic code |
| `true` | error | `failed`, the error's code |
| any | a dependency is inactive | the dependency's category |

An active provider goes into `Selection.Active`; an inactive one contributes
one `CapabilityState` per declared capability at scope `workspace` to
`Selection.Inactive`. A `Required` provider that ends inactive is an error:
no generation can be built without it. `auto` means "use an already approved
profile when one is available" (Section 20.2); it never executes something
found on `PATH`, and detection that would run a tool requires trust first.

## Sink: bounded ownership transfer

The provider's `Sink` is a `BatchSink` over storage's `UnitWriter`. It owns
every record from acceptance until the writer has persisted it. The bounds
come from configuration: `index.batch_records`, `index.batch_bytes`,
`resources.max_provider_record_bytes` (per-batch `Limits`) and
`index.queue_bytes` (the shared `Pool` of retained bytes across every sink of
one indexing run).

- **Reserve before accept.** A record's bytes are charged against the pool
  before it is queued. `Reserve(ctx, n)` lets a provider charge the size of
  input it is about to decode or buffer, so protobuf and CSV decoders reserve
  before allocating.
- **Flush at the smaller limit.** A batch flushes as soon as the next record
  would exceed *either* `BatchRecords` or `BatchBytes`. The batch that is
  written contains exactly the records that fit.
- **Block, never exceed.** When the pool is exhausted a call charges or
  subscribes to the next release under one pool lock (no release can slip
  between the two), then asks every live sink — its own included — to persist
  what it has queued, and only then waits. It never waits while holding a sink
  lock and returns promptly with `CTX_CANCELED` when the context ends.
- **No bypass.** A single record over `max_provider_record_bytes` is
  `CTX_RESOURCE_LIMIT` with `Details["limit"]` naming the bound. Nothing is
  queued or written.
- **Failure cancels producers.** A failed write latches the sink, drops every
  queued batch and returns its bytes to the pool in one release, and cancels
  the unit's context, so every goroutine producing into that unit stops.
  Later calls return the latched error.
- **Discard on every path.** `Discard` drops whatever is still queued,
  returns its bytes and removes the sink from the pool's live set. It is
  idempotent and a no-op after a clean `Flush`. `RunUnit` defers it, so a
  provider that erred or was canceled with records queued never shrinks the
  pool for the rest of the run.
- **Reference order.** Nodes are always flushed before relations, aliases and
  search documents, because those rows reference node identities the same
  unit may have minted. Within a batch records are sorted by their stable key
  so the transaction shape depends on content, not on worker timing.

Byte accounting is the documented deterministic size function in `sink.go`
(`NodeFactBytes`, `RelationFactBytes`, `AliasBytes`, `SearchUnitBytes`): a
fixed overhead per record and per evidence row plus every string the record
carries (including `Node.SemanticSource` and `NodeFact.CanonicalKey`). It is a
superset of the estimate `UnitWriter` applies, so a batch the sink admits is
never refused by a writer configured with the same bounds.

Ownership: a slice handed to a `Put` method belongs to the sink afterwards.
The provider must not retain or mutate it.

Sizing: a producer holds at most one outstanding `Reserve` while it calls
`Put` for the decoded record, so `queue_bytes ≥ concurrency × 2 ×
max_provider_record_bytes` is the minimum that guarantees every producer can
always make progress; the coordinator bounds concurrency accordingly. Because
an acquirer flushes the other live sinks' queued batches before it waits, a
sink that has stopped producing with a half-filled batch cannot pin the pool:
its queued bytes are persisted (under its own lock and its own run context,
so a failure belongs to its unit) and returned. What the rule above still
bounds is bytes held by reservations that are in use, which no other party
may release.

## Running a unit: sealed after validation or deleted

`RunUnit(ctx, provider, request, output, limits, pool)` is the one path from a
provider to a sealed unit. It runs `IndexUnit` under a cancellable child
context, flushes the sink, checks that the result names this run and reports
`succeeded`, and only then calls `Seal`, which is storage's whole-unit
validation (evidence behind every fact, ranges inside their source, endpoints
and alias targets visible through the dependency closure) followed by the
state flip and generation membership. On any other outcome — provider error,
write failure, cancellation, timeout, or a `partial`/`failed` result — the
unit is failed: `UnitWriter.Fail` deletes every row it wrote. Failed partial
output is never attached to a generation and can never be queried.

The returned `ProviderResult` always names the run and a terminal state. A
write failure that cancelled the provider is reported as `failed` with the
write error, not as `canceled`. An expired deadline is `timed_out` whether it
arrives as a bare `context.DeadlineExceeded` or typed through
`model.Canceled`; that check precedes the error-code mapping. `RunUnit` does
not complete the provider run: the caller (the Task 12 coordinator) reports
the aggregate over a provider's units with `Store.CompleteProviderRun`,
passing `provider.CodeOf(err)` as the diagnostic code.

Misconfigured bounds — a non-positive limit, a record bound above the batch
bound, a batch bound above the pool capacity — are `CTX_ARGUMENT_INVALID` at
construction, not resource exhaustion.

Reuse is decided before `BeginUnit`: the coordinator derives the unit key
(`model.NewUnitID` over provider, version, scope, config hash, input digest
and dependency digest), asks `Store.UnitState`, and attaches a sealed unit
with `AttachUnit` instead of rebuilding it. Storage verifies that every input
of a reused unit is exactly the selected snapshot's version of that file.

## Identity resolution

`internal/reconcile` owns resolution policy. A provider builds a
`model.NodeCandidate` (provider, scope, native key, optional strong key, kind,
language, name, qualified name, signature, file, hash, range) and calls
`Resolver.Resolve`. The answer is a `model.Resolution`: the canonical `Node`,
the `MatchBasis`, the `CanonicalKey` the node's ID derives from and a bounded
`Ambiguous` list. The provider copies `CanonicalKey` into
`NodeFact.CanonicalKey` verbatim; storage recomputes the node ID from it and
rejects a mismatch. Providers never derive canonical keys themselves.

Resolution order (Section 9.4):

1. **Strong key, then native key, against persisted aliases.** The resolver
   looks up `(ScopeKey, StrongKey)` and then `(ScopeKey, NativeKey)` in the
   `native_aliases` of the unit's **sealed declared dependencies only**
   (`Store.LookupAliases`, bounded at `MaxAliasLookup`, read-only, one short
   transaction). A hit is basis `native_key`. The identity with the smallest
   canonical key is primary; the rest, in the same order, form the bounded
   `Ambiguous` list the provider records as `may_refer_to` edges. The lookup
   fetches one row more than a `Resolution` can hold; more equally supported
   identities than `MaxAmbiguousCandidates` is `CTX_PROVIDER_OUTPUT_INVALID`
   with `Details["limit"]`, never a silent truncation. An alias match adopts
   the stored kind, because the identity already exists.
2. **Minted identity** by `reconcile.CanonicalKey(candidate)`, the single
   implementation of Section 9.1's `canonical_entity_key`:
   - file and range present → `source_location`: key over the file's path
     identity, name, kind, language, qualified name and exact byte range;
   - qualified name and file, no range → `qualified_signature`: key over
     scope, qualified name, kind, language, signature and file;
   - qualified name, no file → `structural_key`: the same tuple without a
     file, for packages, modules and dependencies that have no location;
   - otherwise → `unresolved`: key over scope, native key, provider and kind.
     It never merges with another entity by short name; the provider marks
     the node's metadata `resolution=unresolved`.

Every answer is a pure function of the candidate and the set of sealed
dependency units. The dependency list is sorted and deduplicated at
construction, the lookup is ordered by canonical key, and nothing consults the
unit being built, another running unit, wall time or scheduling. A
source-equivalent unit therefore yields identical identities no matter in
which order its dependencies completed.

## Conformance harness

`internal/provider/providertest` is the shared fixture Tasks 7–11 build on.
`New(t, files)` stands up the real store, CAS, snapshot view, staging
generation and confined workspace root over a small file set; `Plan`, `Begin`
and `Run` drive a unit through exactly the production `BeginUnit`, sink,
resolver, seal and fail paths; `Func` is a provider assembled from functions;
`Recorder` captures the identities a run persisted; `Conform(t, p, files,
scope, inputs)` checks the descriptor, detection, a succeeded sealed unit and
that a second run over an identical repository yields the same unit identity
and the same persisted identities.
