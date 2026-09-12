# SCIP provider

`internal/provider/scip` is the Section 11.4 provider: it imports SCIP
precise indexes as `compiler`-precision facts. It has two inputs and one
decoder.

| Input | Unit scope key | Source binding |
|---|---|---|
| A supplied `.scip` file inside the snapshot (`Options.Import`, the explicit import request field), optionally with an input-hash manifest (`Options.Manifest`) | `import:<root-relative path>` (`scip.ImportScope`) | `verified` only when every document that names a snapshot file proves its bytes; otherwise `unverified` |
| An approved installed indexer profile (`[analyzers.scip-go]`, `[analyzers.scip-typescript]`, `[analyzers.scip-java]`) run by codectx against a private materialization of the snapshot | `profile:<profile name>` (`scip.ProfileScope`) | `verified` by construction: the run binds its inputs by hash, and the digest of that input manifest is reported on every diagnostic the profile path returns |

The descriptor is `scip`, version `1`, capabilities `precise_definitions`,
`precise_references`, `precise_implementations`, invalidation scope
`workspace`, optional. It depends on `filesystem` and `treesitter`, plus
`manifest` when a profile is configured (profiles read package manifests).

## Coordinator contract

The coordinator of Section 11.1 (Task 12, `internal/app`) is the consumer of
every exported name here: `scip.New`, `Descriptor`, `Detect`,
`Scopes`/`ImportScope`/`ProfileScope`, `Verify` and `IndexUnit`. Nothing else
in the tree calls them until that coordinator exists.

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

## Positions and binding

Occurrence coordinates are converted with `internal/source` against the exact
pinned bytes in the document's declared `position_encoding` (UTF-8, UTF-16 or
UTF-32). An unspecified encoding is never guessed: the document is skipped
and the capabilities are `partial` with `CTX_PROVIDER_OUTPUT_INVALID`. A
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
| symbol never defined in the index (external) | as above | unlocated: structural key over scope, qualified name, kind; metadata `"scip_external":true`; evidence is the referencing occurrence |
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
| role `WriteAccess` | `writes` | same | the occurrence |
| role `ReadAccess` | `reads` | same | the occurrence |
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

## Profiles (ruling R9-2: unverified against a real tool)

No `scip-go`, `scip-typescript` or `scip-java` binary was available on the
development machine, so the profiles are implemented completely but have
**not** been run against a real indexer; the exact argument arrays below are
the documented invocations of each tool and must be confirmed by a real run
before a profile is described as working.

A profile is a `[analyzers.<kind>]` table whose name is the kind. It is
executed through the shared `internal/process` runner with an argv array
only (ruling R9-3: no shell anywhere), with the typed substitutions
`${input_dir}` (the materialization root, also the working directory),
`${output_file}` (a private file under the run directory), `${work_dir}` and
`${manifest}`, applied in a fixed order so one configuration always yields one
argv. The child environment is exactly the allowlisted variables.

`${manifest}` is the path codectx writes the run's input manifest to. That
file is written **after** the tool exits, because its second line commits to
the SHA-256 of the index the tool produced, so a tool cannot read it during
its own run; the placeholder is still substituted because an unsubstituted one
would reach the child as the literal text `${manifest}`. The run is bounded by the smaller of the profile timeout and
`providers.scip.timeout`, by the profile's memory and disk budgets, and by 1
MiB of captured output per stream. After the run the output must be a
regular file within `MaxIndexBytes`, its metadata must name the profile's
tool, and the tool version must satisfy `version_constraint` (an exact
version or space-separated comparators such as `>=0.1.20 <0.2.0`); a
mismatch is `CTX_TRUST_REQUIRED`. A profile with no `version_constraint` is
refused outright: an unconstrained tool is not an approval.

Once the run has produced its index, codectx writes the run's input manifest
under the run directory in the v1 format above — including the SHA-256 of the
index the run just produced, which is why it cannot be written before the run.
The file dies with the run directory; the durable records are the manifest's
own digest, reported as `input_manifest_sha256` on every diagnostic this path
returns, and `UnitSpec.InputHash`, which the coordinator computes over the
unit's declared inputs.

**Executable trust.** A configured `checksum` is verified against the
executable immediately before it runs. A profile **without** a `checksum` runs
whatever binary is at the configured path at that moment: the absolute path is
the whole of the approval. Even with a checksum, the digest is computed by
reading the file and the kernel then executes the path again, so a writable
path can be swapped between the hash and the `execve` (a time-of-check to
time-of-use window). Approve tools at paths only a trusted account can write,
and prefer a `checksum` so an ordinary substitution is detected.

**Network.** `network` is a **declared posture that codectx records and does
not enforce**. There is no sandbox behind it: an indexer run under
`network = "denied"` can still reach the network, because resolving
dependencies is what several of these tools do. The value is the posture the
profile was approved under; it is carried on every diagnostic the profile path
returns (`network=<value>`) so a report says what was approved, and
`network = "allowed"` is the user's explicit opt-in, never a refusal.

```toml
[analyzers.scip-go]
executable = "/usr/local/bin/scip-go"
version_constraint = ">=0.1.20 <0.2.0"
args = ["--output", "${output_file}"]
env_allowlist = ["HOME", "PATH", "GOPATH", "GOMODCACHE", "GOCACHE", "GOFLAGS"]
work_dir = "/home/me/.cache/codectx/scip"
memory_budget_bytes = 2147483648
disk_budget_bytes = 4294967296
network = "denied"
timeout = "20m"

[analyzers.scip-typescript]
executable = "/usr/local/bin/scip-typescript"
version_constraint = ">=0.3.0 <0.4.0"
args = ["index", "--output", "${output_file}"]
env_allowlist = ["HOME", "PATH"]
# ...

[analyzers.scip-java]
executable = "/usr/local/bin/scip-java"
version_constraint = ">=0.10.0 <0.11.0"
args = ["index", "--output", "${output_file}"]
env_allowlist = ["HOME", "PATH", "JAVA_HOME"]
# ...
```

Materializations, manifests and outputs live under the profile's `work_dir`
in a per-run directory that is removed on success, failure and cancellation.
