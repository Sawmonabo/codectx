# SCIP provider

`internal/provider/scip` is the Section 11.4 provider: it imports SCIP
precise indexes as `compiler`-precision facts. It has two inputs and one
decoder.

| Input | Unit scope key | Source binding |
|---|---|---|
| A supplied `.scip` file inside the snapshot (`Options.Import`, the explicit import request field), optionally with an input-hash manifest (`Options.Manifest`) | `import:<root-relative path>` (`scip.ImportScope`) | `verified` only when every document that names a snapshot file proves its bytes; otherwise `unverified` |
| One of the six managed indexer profiles (`scip-go`, `scip-typescript`, `scip-python`, `scip-java`, `rust-analyzer`, `scip-clang`), whose pinned payload codectx runs against a private materialization of the snapshot | `profile:<profile name>` (`scip.ProfileScope`) | `verified` by construction: the run binds its inputs by hash, and the digest of that input manifest is reported on every diagnostic the profile path returns |

The descriptor is `scip`, version `3`, capabilities `precise_definitions`,
`precise_references`, `precise_implementations`, invalidation scope
`workspace`, optional. It depends on `filesystem` and `treesitter`, plus
`manifest` when a profile is configured (profiles read package manifests).

## Coordinator contract

The coordinator of Section 11.1 (Task 12, `internal/app`) is the consumer of
every exported name here: `scip.New`, `Descriptor`, `Detect`,
`Scopes`/`ImportScope`/`ProfileScope`, `Verify`, `IndexUnit`, and the delta
form `Import` with `ImportOptions`, `Report`, `DocumentManifest`, `Delta`,
`Change` and `Class`. Nothing else in the tree calls them until that
coordinator exists.

- `Detect` inspects declared inputs only through the confined root: the
  import path, and the trigger manifests of each profile (`go.mod`;
  `package.json`/`tsconfig.json`; `pom.xml`/`build.gradle`/`build.gradle.kts`)
  together with the profile executable's existence. It never runs a tool.
  Nothing usable is `CTX_PROVIDER_UNAVAILABLE`.
- `Scopes(detection)` lists the unit scope keys to plan. A profile whose
  executable is not installed plans no unit, and `runProfile` refuses with
  `CTX_PROVIDER_UNAVAILABLE` before anything is materialized, so a missing
  tool never costs a snapshot materialization or a failed unit.
- `Verify(ctx, view, scopeKey)` decides `UnitBuild.SourceBinding` before the
  unit is opened. `IndexUnit` repeats the check and reports it in its
  capability states, so a unit opened as verified whose index is not is
  visibly `partial` with `CTX_SOURCE_BINDING_UNVERIFIED`, never silently
  exact.
- Declare every eligible snapshot file of the workspace as the unit's
  inputs: a fact may name any file the index describes, and storage refuses
  a fact on an undeclared file. Documents the snapshot does not hold are
  skipped and counted.
- `Import(ctx, req, sink, opts)` is `IndexUnit` plus the refresh: `opts.Previous`
  is the sealed unit's stored `DocumentManifest` (nil is a full import) and the
  `Report` carries the delta, the fresh manifest and every count of what the
  import did not admit. `IndexUnit` is `Import` with no previous manifest, which
  is all the `provider.Provider` interface can express.

## Decoding

Top-level and nested protobuf wire fields are walked by a bounded reader
(`wire.go`): a length prefix is checked against the enclosing message before
anything is sized by it; one metadata, occurrence or symbol record is read
into a reusable buffer bounded by `Limits.MaxRecordBytes`; a `Document` of
any size is walked field by field, its `text` streamed through SHA-256 and
never held; documents and occurrences per document are counted against
bounds; groups are refused. Nothing calls `io.ReadAll` or unmarshals the
index.

The generated Go bindings live in a nested Go module
(`github.com/scip-code/scip/bindings/go/scip`) that the pinned root module
`github.com/scip-code/scip v0.10.0` does not contain, so `decode.go` decodes
the handful of records this provider needs (Metadata, Document fields,
Occurrence, SymbolInformation, Relationship) against the field numbers of
`scip.proto` in that module. The checked-in fixture was produced with the
real bindings, so the decoder is verified against library-encoded bytes.

The import is three streaming passes over the index:

1. **Binding pre-pass**: document paths and text hashes decide the binding
   before any fact exists (the binding decides whether a bad coordinate
   fails the unit or is skipped).
2. **Definitions**: occurrences and symbols are spooled to a private scratch
   SQLite file under the work directory (bounded by `MaxSpoolBytes`, 4 MiB
   page cache) as they stream; when a document ends its position encoding is
   known, so its definitions are converted, resolved, published, aliased and
   recorded in the on-disk symbol map.
3. **References**: read back from the spool with the map complete, so a
   forward or external reference binds to the identity its definition
   minted. Edges are grouped on disk by relation identity and published with
   one evidence row per distinct occurrence range.

The scratch directory is removed on every path.

## Delta import

A managed indexer always re-indexes its whole unit — only `scip-java` has a
per-file pipeline, and narrowing `scip-typescript` or `scip-python` changes
symbol names — but the importer never rewrites the whole unit. SCIP documents
are independent and global symbol strings are position-free, so a refreshed
unit is applied as a per-document delta (Section 11.4, "Delta import";
`docs/research/12-incremental-scip-lsp.md` Sections 3.3–3.5 and 6.2).

**Document manifest.** `DocumentManifest` is the sorted `(root-relative path,
canonical document hash)` list of one sealed unit, held as a file rather than a
Go slice because a unit may describe up to `MaxDocuments` documents and Section
6 forbids a whole-repository list in the heap. Its format is

```
codectx-scip-documents v1
<64-hex canonical document hash>  <root-relative path>
...
```

sorted by path with strictly increasing paths. `Provider.Version` moves with
the hash domain, so a manifest written under an earlier mapping is never
reachable: the old unit has a different `UnitID` and is not reused. `LoadDocumentManifest` validates
all of that and refuses a malformed file rather than treating it as an empty
previous state, because an empty previous state silently imports everything.
`Save` writes through a temporary file and one rename; `Close` removes the
private temporary an import produced. `Diff(prev, fn)` is a merge join over the
two sorted files: it returns the `Delta` counts and streams each path to `fn`
as `changed`, `removed` or `unchanged`. The sets stream rather than return as
slices for the same heap reason; `fn` may be nil when only the counts are
wanted.

**Canonical document hash.** Framed with `model.NewHasher("scip-document-v1")`
over the document's path, language, resolved position encoding, **the pinned
file's content hash**, and the canonical digest of every occurrence and symbol
record of the document, folded in sorted order.

- Occurrence and symbol digests cover exactly the fields this provider turns
  into facts, in the normalized four-value range form. A producer that switches
  between the deprecated packed range and the typed single/multi-line range, or
  that emits occurrences in a different order, does not report a changed
  document; a field the provider ignores cannot report one either.
- Symbol relationships are sorted, because they are a set. A document can
  change by exactly one relationship on one symbol with an identical occurrence
  and symbol count, on a file nobody edited: adding a method to an interface
  its implementers already satisfy changes the implementing file's document
  (measured, case L). The changed set therefore comes from the hashes, never
  from a git diff.
- `Document.text` is excluded: no indexer emits it (measured: zero of six) and
  it is not a fact this provider publishes.
- The pinned content hash is in the hash because a document can be byte
  identical while its source file changed — a trailing newline, a comment
  edited after the last declaration. Without it those bytes would be classified
  unchanged and the retained evidence rows would name content hashes the
  snapshot no longer holds, which is the one way a delta import can serve wrong
  source as compiler evidence.

**What a delta run publishes.** Facts for the changed documents only.
Definitions are converted and resolved for *every* admitted document, because a
definition's canonical key is its file and byte range, so an unchanged
document's definitions must be re-derived here or a reference from a changed
document to a symbol defined in an unchanged one would mint a second, unlocated
identity instead of naming the stored node. The delta therefore buys the
storage, FTS and reconcile cost of the unchanged documents — which is where
almost all of the import cost lives — and not their position-conversion cost.

**Removal is set-based.** A deleted source file yields no `Document` at all, not
an empty one, so paths in the stored manifest that the fresh index no longer
describes are `removed`. A rename is a new `FileID` (Section 9.4), so every
`git mv` takes this path.

**Rejected and superseded documents.** A document whose `relative_path` escapes
the project root is rejected and counted in `Report.OutsideRoot`: 18 of the 141
documents `scip-go` emits for this repository are the `go test` mains it writes
under `$GOCACHE`, whose paths are `../../../../..`-style escapes into a
content-addressed build cache. Admitting them would bake absolute machine paths
into the index, churn about 13% of the document set for unrelated reasons, and
produce paths that can never join a repository `FileID`. `scip.proto` calls
`relative_path` unique but defines nothing for a duplicate, and its own two
reference consumers disagree (`FlattenDocuments` unions, `expt-convert` keeps
the first), so this importer picks one rule and counts it in
`Report.DuplicatePaths`: the last document of a path wins and the earlier ones
are dropped. Neither is a capability degradation; both are reported counts.

**Index-level state.** `Index.external_symbols` is an `Index` field, not a
`Document` field: it belongs to no path, contributes to no document hash and is
re-imported on every refresh. The same holds for the node of a symbol this
index never defines — it has no source location, so its evidence carries no
file and no range (Section 9.3), and it belongs to the index-level bucket
rather than to whichever document happened to reference it first. The
referencing location is not lost; it is on the reference edge, which is where
it belongs.

## Positions and binding

Occurrence coordinates are converted with `internal/source` against the exact
pinned bytes in the document's `position_encoding` (UTF-8, UTF-16 or UTF-32).

Five of the six managed indexers leave that field unspecified, so a document
that does not declare an encoding is converted in the measured encoding of the
**tool build** — the `Index.metadata.tool_info` name *and* version — that wrote
the index. Measured on this machine against a fixture whose line carries a
4-byte and a 2-byte rune before an identifier, decoding every occurrence range
under all three encodings:

| Tool build | `Document.position_encoding` | Columns actually are |
|---|---|---|
| `scip-go` 0.2.7 | absent | UTF-8 |
| `scip-clang` 0.4.0 | absent | UTF-8 |
| `scip-typescript` 0.4.0 | absent | **UTF-16** |
| `scip-python` 0.6.6 | absent | **UTF-16** |
| `scip-java` 0.0.0-SNAPSHOT | absent | **UTF-16** |
| `rust-analyzer` 1.98.0 | `UTF8` | UTF-8 — declared, never assumed |

`scip-python` is UTF-16 rather than the UTF-32 `scip.proto` suggests for Python
indexers, because it is a TypeScript program (a pyright fork) — which is why
the table is measured rather than read off the proto's advice. `rust-analyzer`
declares its encoding on every document, so it is not in the fallback table at
all: a build that stopped declaring it would be an unmeasured pair and its
documents would be skipped.

The key is the name *and* the version, and the version comparison ignores a
leading `v` and any build or pre-release suffix. A table keyed on the name
alone would be an assertion about every build a tool will ever have, and a
wrong encoding does not announce itself: reading a UTF-16 column as a byte
offset lands inside a UTF-8 sequence only by luck and far more often selects a
valid, in-range, rune-aligned extent a few bytes off the identifier, which is
then published at `compiler` precision against source that is not the symbol.

An assumed encoding is therefore **proved once per document before any of its
occurrences is admitted**: the first definition occurrence whose symbol names
an identifier the source spells literally must select exactly that identifier.
Namespace descriptors (package, module and file paths), meta descriptors
(Python's `__init__`), backtick-escaped names (`<init>`, operators) and `local`
symbols carry no such name and are passed over; a document in which none of
the first 256 definitions carries one is skipped rather than admitted on an
unchecked guess. On a line with a non-ASCII rune before the token the readings
disagree and a wrong guess is caught; on an ASCII-only line every reading
converts to the same bytes, so there is nothing to catch. A document whose
guess does not hold is skipped and the capabilities are `partial` with
`CTX_PROVIDER_OUTPUT_INVALID`.

`Metadata.text_document_encoding` is deliberately never consulted. All six
indexers set it to `UTF8`, including the three whose columns are UTF-16,
because `scip.proto` defines it as the encoding of the source files on disk and
says it is unrelated to ranges. Reading it as a position encoding would convert
every UTF-16 column as a byte offset.

A document of any other tool build that does not declare an encoding is
skipped and the capabilities are `partial` with `CTX_PROVIDER_OUTPUT_INVALID`:
Section 9.3 forbids guessing one. `Report.AssumedPositionEncoding` counts the
documents that were converted through the per-tool-build table **and** proved
against their pinned bytes. A
coordinate that does not land on the bytes is `CTX_PROVIDER_OUTPUT_INVALID`
and fails the unit under a verified binding (the index claims to describe
these bytes and does not); under an unverified binding it is skipped and
counted.

A document proves its bytes by embedded `text` whose SHA-256 equals the
pinned content hash, or by a matching row of a **qualifying** input-hash
manifest.

A bare list of `<sha256>  <path>` lines does not qualify. Nothing in such a
list ties it to the index being imported, so the same list would "prove" any
index at all: it is a user assertion, not a verification. A supplied manifest
verifies only when

```
codectx-scip-manifest v1
index-sha256 <hex sha256 of the exact .scip bytes being imported>
<sha256>  <root-relative path>
...
```

its first line is that header, its second names the SHA-256 of the exact index
bytes this unit imports, and **every** row names a file the snapshot holds at
exactly that content hash. A manifest that fails any of these proves nothing:
the import continues with `CTX_SOURCE_BINDING_UNVERIFIED`, it is never a hard
failure. A codectx-invoked profile writes exactly this format.

Verification is decided over the documents that name snapshot files. A
document whose `relative_path` the snapshot does not hold abstains: it neither
proves nor disproves the binding, and it is skipped and counted when facts are
emitted. A supplied index is `verified` only when at least one in-snapshot
document is proven and none of them fails; an index with no proven document is
`unverified`. Unverified facts are still published for discovery, but the unit
carries `source_binding=unverified`, every node's metadata says
`"source_binding":"unverified"`, and every capability is `partial` with
`CTX_SOURCE_BINDING_UNVERIFIED`. Project root, matching paths, timestamps and
tool versions prove nothing.

## Identity and alias scopes (ruling R9-1)

Every node goes through `req.Resolver`; the provider copies
`Resolution.CanonicalKey` verbatim.

| Symbol | Alias scope key | Candidate |
|---|---|---|
| `local N` | `file:<path>` | located (file + definition range), no qualified name |
| global | `pkg:<manager> <package-name> <version>` from the symbol's package descriptor | located; qualified name = the raw descriptor string (`pkg/Foo().`) |
| symbol never defined in the index (external) | as above | unlocated: structural key over scope, qualified name, kind; metadata `"scip_external":true`; evidence carries no file and no range, because the entity has no source location in this index and Section 9.3 requires an absent range rather than a stand-in. The referencing location is on the reference edge |
| the file node a file-level occurrence points out of | `workspace`, native key `file:<path>` | exactly the filesystem provider's file candidate: kind `file`, name `path.Base`, qualified name = path, no defining file, no range, no language. Resolving it against the declared `filesystem` dependency adopts that provider's identity instead of minting a second file node for one path; SCIP's own evidence row (`[0, size)` of the pinned file) stays on it |

Name is `display_name`, else the last descriptor's name. Signature is
`signature_documentation.text` when it fits the signature ceiling.
Ambiguous resolutions become `may_refer_to` edges.

Node kind is `SymbolInformation.Kind` mapped to Section 9.2: Class, Object,
SingletonClass, Mixin, Concept and the bare type kinds (Type, TypeAlias,
AssociatedType, TypeFamily, TypeParameter, DataFamily) → `class`; Interface,
Protocol, Trait, TypeClass → `interface`; Struct, Union, Message → `struct`;
Enum → `enum`; EnumMember, Constant → `constant`; Field, Property,
StaticField, StaticProperty, Attribute, StaticDataMember, Key → `field`;
Function, Macro, Constructor, Lemma, Theorem → `function`; every method-like
kind → `method`; Variable, Parameter, SelfParameter, ThisParameter,
StaticVariable, Value, Instance → `variable`; Package, PackageObject,
Library → `package`; Module → `module`; Namespace → `namespace`; File →
`file`. An unspecified kind falls back to the descriptor suffix (`/`
namespace, `#` class, `().` method under a type else function, `.` variable,
`:` field, `!` function).

## Relations and evidence

| SCIP | Relation | From | Evidence range |
|---|---|---|---|
| occurrence with role `Import` | `imports` | innermost definition whose enclosing range contains the occurrence, else the document's file node | the occurrence |
| any other non-definition occurrence | `references` | same | the occurrence |
| relationship `is_implementation` | `implements` | the symbol's definition node | the symbol's definition |
| relationship `is_reference` or `is_type_definition` | `references` | same | the symbol's definition |
| relationship `is_definition` | none (it names the symbol's own definition) | | |

Every evidence row is `compiler` precision with the SCIP symbol as
`native_key` and the role word as `detail`. The same edge at two ranges is
one relation with two evidence rows; the same edge at the same range is one
row. One relation carries at most `model.MaxEvidencePerFact` (64) evidence
rows; further occurrences are counted and the capabilities are `partial`
with `CTX_RESOURCE_LIMIT`. The first definition of a symbol is the one
references bind to; a later definition keeps its own located identity.

The occurrence roles `ReadAccess` and `WriteAccess` are deliberately not
mapped. `reads` and `writes` are the `dependence` provider's facts, derived
from the graph's assignment operators (Section 11.6), and two providers
publishing one relation kind from different precisions is the parallel
implementation policy forbids. A read or write occurrence is a `references`
edge here, like any other non-definition, non-import occurrence. No indexer
measured here distinguishes a call-site reference from a reference to a
function value either; the call identification SCIP cannot do is the
call-site join below.

## Call-site join (Section 11.3)

Every non-definition occurrence publishes an alias on the symbol it resolves
to:

| | |
|---|---|
| scope key | `file:<path>` |
| native key | `callsite:<path>:<start>-<end>`, the **one-based inclusive** byte range of the occurrence, i.e. `start+1` and `end` of the zero-based half-open `model.SourceRange` |

The identifier `Bar` occupying zero-based bytes `[34,37)` of `pkg/a.go` is
therefore scope `file:pkg/a.go`, key `callsite:pkg/a.go:35-37`. The tree-sitter
provider publishes the identical key on the syntactic callee of every call site
it finds, so the reconciler merges the two identities wherever the ranges are
equal and the `calls` relation acquires a compiler-precision target while both
evidence rows survive. `callsite.go` is the only place this key is spelled.

The alias is published for every non-definition occurrence rather than a
guessed subset, because SCIP cannot tell a call-site reference from a reference
to a function value — no indexer distinguishes them and none sets a write role.
An alias at a range where tree-sitter found no call site is inert: the join is
exact-range equality, so it aliases the symbol to a key nothing else names. A
key that would exceed `model.MaxNativeKeyBytes` or a scope key that would
exceed `model.MaxScopeKeyBytes` is skipped and counted in
`Report.SkippedCallsiteAliases`, never truncated into a key that would join the
wrong range.

## Bounds

`scip.Limits` (defaults in `DefaultLimits`): whole index 1 GiB; one record
`resources.max_provider_record_bytes` (4 MiB); 1,000,000 documents; 4,000,000
occurrences per document; 4 GiB spooled; one source file 5 MiB (a larger file
is skipped, `partial` with `CTX_RESOURCE_LIMIT`); materialization 4 GiB;
manifest 64 MiB.

The record buffer — the only buffer sized by untrusted index bytes — is
charged against the sink's byte pool through `Reserve` when the sink offers
it. One document's source bytes are **not** charged: `BatchSink.Reserve`
refuses a reservation above `resources.max_provider_record_bytes` (4 MiB),
while a source file is bounded by `MaxSourceFileBytes` (5 MiB), so the charge
is impossible for exactly the largest case. At most one document's source is
held at a time, so a unit retains up to `MaxSourceFileBytes` (5 MiB) outside
the pool's accounting, on top of the pool's own budget. That buffer is sized
by the pinned snapshot file, whose size is checked before the read.

## Profiles

A profile is one of the six managed indexers. It is **product code, not
configuration**: the tool it starts, its argument array, the parent environment
variables it may see, its budgets, its timeout and its declared network posture
are constants of this build, and the binary is the payload the embedded tool
lock pinned and `internal/toolchain` verified. There is no `[analyzers.<name>]`
table, no approved path and no PATH lookup (Section 20.2 — trust is the lock).

Every one of the six was resolved through the real lock and store and run end
to end on `linux/amd64` — materialize, index, import, seal — against a fixture
of its own language; the exact argv, the environment allowlist and the sealed
result of each run are in the lane B1 report. The six profiles and the argument
arrays this build pins, after the payload's own launcher prefix:

| Profile | Triggers | Arguments after the launcher | Environment allowlist | Network posture |
|---|---|---|---|---|
| `scip-go` | `go.mod`, `go.work` | `index --output <output>` | `PATH HOME GOPATH GOCACHE GOMODCACHE GOFLAGS GOPROXY GOPRIVATE` | allowed |
| `scip-typescript` | `tsconfig.json`, `jsconfig.json`, `package.json` | `index --cwd <input> --output <output> --no-progress-bar` | `HOME` | denied |
| `scip-python` | `pyproject.toml`, `setup.py`, `setup.cfg`, `requirements.txt` | `index --cwd <input> --output <output> --project-version 0.0.0 --quiet` | `PATH HOME` | denied |
| `scip-java` | `pom.xml`, `build.gradle`, `build.gradle.kts` | `index --output <output>` | `PATH HOME MAVEN_OPTS GRADLE_USER_HOME` | allowed |
| `rust-analyzer` | `Cargo.toml` | `scip <input> --output <output>` | `PATH HOME CARGO_HOME RUSTUP_HOME` | allowed |
| `scip-clang` | `compile_commands.json`, `compile_flags.txt` | `--compdb-path=<input>/compile_commands.json --index-output-path=<output>` | `PATH HOME` | denied |

`<input>` is the private materialization root, which is also the child's
working directory; `<output>` is a file under the run directory, outside the
materialization. The launcher prefix comes from the lock: a self-contained
binary runs as itself, a Node-hosted indexer runs as `<managed node> <entry>`,
and `scip-java`'s launcher runs with `JAVA_HOME` pointing at the managed JDK.
The child's environment is exactly the allowlisted variables the parent has
plus the variables the payload needs; the payload's come last, so a host
`JAVA_HOME` can never shadow the pinned runtime.

Everything is executed through the shared `internal/process` runner with an
argv array only (ruling R9-3: no shell anywhere). The run is bounded by the
smaller of the profile's own timeout and `providers.scip.timeout`, by the
profile's memory and disk reservations, and by 1 MiB of captured output per
stream.

**Two pinned arguments exist because of a measured failure, not a preference.**
`scip-python` is given `--project-version` because, left to itself, it asks git
for the current revision; the private materialization is never a repository, so
the lookup fails, the version stays undefined and the indexer dies inside its
symbol constructor having written nothing. The value is a constant because it
is part of every symbol string the indexer emits — deriving it from the
snapshot would rename every symbol on every commit and no fact would ever be
reusable. `scip-clang` reads a compilation database whose `directory` fields
are absolute paths in the tree that generated it; inside the materialization
those paths do not exist and the indexing worker crashes. The database **in the
private copy** is therefore normalized before the run: each entry's directory
is moved by the entries' common prefix onto the materialization root. The
repository is never touched, and a database that is absent, unreadable,
oversized, not a JSON array or already relative is left exactly as it is.

**Tool identity.** The index's `tool_info.name` must name the profile's own
tool; output from anything else is `CTX_PROVIDER_OUTPUT_INVALID`. The tool's
self-reported *version* is deliberately not compared against anything: the
lock's entry digest identifies these bytes, and two of the six payloads report
a version no lock-derived constraint could match — `scip-java` 0.13.1 reports
`0.0.0-SNAPSHOT` and the `rust-analyzer` release tagged `2026-08-17.4` reports
`1.98.0 (88d9e12 2026-08-18)`. A constraint written to accept those accepts
anything. The reported version is kept as provenance: it selects the measured
position encoding (table above) and reaches the unit's identity through the
payload fingerprint.

**Tool identity reaches the unit key.** `[tools]` is deliberately outside
`AnalysisConfigHash`, so `Descriptor().Version` is `3/<digest>` where the digest
covers every profile payload's `Tool.Fingerprint()` in a fixed kind order.
Replacing an indexer therefore makes every unit it produced unreachable instead
of silently reusable. A kind whose payload did not resolve contributes one fixed
empty slot, never the reason it is missing: two machines that resolved the same
pinned payloads must key their units identically, and whether some *other*
language's indexer is absent because the store is empty or because `tools.offline`
is set is not part of what produced these facts. That reason is carried by
`Detection.DiagnosticCode` instead. `Detection.ObservedVersion` carries the same
`3/<digest>` value for the coordinator to fold in — the digest rather than a list
of fingerprints, because `ObservedVersion` is bounded at 256 bytes and six
82-byte fingerprints would be truncated there, silently dropping whichever
payloads sort last. A build that resolved no payload at all carries no digest:
its version is plain `3`. The cost is named rather than hidden: an `import:`
unit's identity also moves when an indexer this repository does not use is
replaced, because the frozen provider contract has one version per provider, not
one per unit.

**False readiness.** Every one of these indexers needs the project's own
dependency context, and several exit 0 after producing a well-formed index that
describes nothing when it is missing. A full profile import that admitted no
record is therefore a typed failure (`CTX_PROVIDER_OUTPUT_INVALID`), not a unit
sealed with three `fresh` capabilities over zero facts. A refresh is exempt: an
unchanged snapshot legitimately emits nothing.

**Absence is typed.** A kind whose payload does not resolve plans no unit and
reports the toolchain's own reason — `CTX_TOOL_OFFLINE`,
`CTX_TOOL_UNSUPPORTED_PLATFORM`, `CTX_TOOL_CORRUPT`,
`CTX_TOOL_OVERRIDE_INVALID`, `CTX_TOOL_DIGEST_MISMATCH`,
`CTX_TOOL_FETCH_FAILED` — on `Detection.DiagnosticCode`, so an operator can
tell "run `codectx tools prefetch`" from "this platform has no payload".

Once the run has produced its index, codectx writes the run's input manifest
under the run directory in the v1 format above — including the SHA-256 of the
index the run just produced, which is why it cannot be written before the run.
The file dies with the run directory; the durable records are the manifest's
own digest, reported as `input_manifest_sha256` on every diagnostic this path
returns, and `UnitSpec.InputHash`, which the coordinator computes over the
unit's declared inputs.

**Network.** The posture is **recorded and not enforced**. There is no sandbox
behind it: an indexer declared `denied` can still reach the network, because
resolving dependencies is what several of these tools do. It is carried on every
diagnostic the profile path returns (`network=<value>`) so a report says what
the run was declared under.

Materializations, manifests and outputs live under the provider's own work
directory (`<work_dir>/profiles/<profile>/`) in a per-run directory that is
removed on success, failure and cancellation.
