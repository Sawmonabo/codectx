# SCIP provider

`internal/provider/scip` is the Section 11.4 provider: it imports SCIP
precise indexes as `compiler`-precision facts. It has two inputs and one
decoder.

| Input | Unit scope key | Source binding |
|---|---|---|
| A supplied `.scip` file inside the snapshot (`Options.Import`, the explicit import request field), optionally with an input-hash manifest (`Options.Manifest`) | `import:<root-relative path>` (`scip.ImportScope`) | `verified` only when every document that names a snapshot file proves its bytes; otherwise `unverified` |
| One of the six managed indexer profiles (`scip-go`, `scip-typescript`, `scip-python`, `scip-java`, `rust-analyzer`, `scip-clang`), whose pinned payload codectx runs against a private materialization of the snapshot | `profile:<profile name>:<project directory>` (`scip.ProfileScope`), one unit per triggering directory | `verified` by construction: the run binds its inputs by hash, and the digest of that input manifest is reported on every diagnostic the profile path returns |

The descriptor is `scip`, version `3`, capabilities `precise_definitions`,
`precise_references`, `precise_implementations`, invalidation scope
`workspace`, optional. It depends on `filesystem` and `treesitter`, plus
`manifest` when a profile is configured (profiles read package manifests).

## Coordinator contract

The coordinator of Section 11.1 (`internal/app`) is the consumer of
every exported name here: `scip.New`, `Descriptor`, `Detect`,
`Scopes`/`ImportScope`/`ProfileScope`, `Verify`, `IndexUnit`, and the delta
form `Import` with `ImportOptions`, `Report`, `DocumentManifest`, `Delta`,
`Change` and `Class`. Nothing else in the tree calls them until that
coordinator exists.

- `Detect` inspects declared inputs only through the confined root: the
  import path, and the trigger manifests of each profile (`go.mod`;
  `package.json`/`tsconfig.json`; `pom.xml`/`build.gradle`/`build.gradle.kts`)
  together with the profile payload's state. It never runs a tool. It walks
  the **whole** workspace for those manifests, not just its root, under the
  snapshot's own exclusion policy, so a dependency directory's manifests are
  never seen and a repository that keeps its projects in subdirectories is
  detected at all. A root-only check reported a monorepo with six indexable
  projects as having none. The walk is bounded like every other detection;
  when the bound cuts the list, the detection says so under
  `unplanned_projects`, because a project dropped in silence is one nobody
  indexes and nobody is told about.
  Nothing usable is `CTX_PROVIDER_UNAVAILABLE`. When at least one language is
  indexable the detection is available, and `Detection.Details` still names
  every other triggered language and why it is not: the toolchain's own
  `CTX_TOOL_*` code for a payload this machine cannot supply, and the marker
  `deferred` for one the first run will fetch. The two are spelled differently
  on purpose — a `CTX_` value is a refusal and the coordinator publishes a
  `partial` capability row for it, while `deferred` is pending work that will
  be indexed at full precision and publishes nothing. Without the details a
  repository with `go.mod` and `Cargo.toml` on a machine with no usable
  `rust-analyzer` would be reported available, plan `scip-go` only, and say
  nothing anywhere about Rust.
- `Scopes(detection)` lists the unit scope keys to plan: `import:<path>` for a
  supplied index, and `profile:<indexer>:<project directory>` for one project
  of one indexer, the empty directory being the workspace root. **One unit per
  triggering directory**, whatever the language and whatever encloses the
  directory. A manifest is the toolchain's own declaration of a boundary, so a
  nested one is a second program and not a subdirectory of the first: a `go.mod`
  inside another module's tree, an `app/package.json` under a root
  `package.json`, and a `tsconfig` inside another project's tree are each their
  own project with their own unit. A workspace root that triggers is a project
  like any other, planned beside the projects below it. What is never done is
  splitting a project at a directory that declares nothing — that split loses
  more than half of the calls that resolve to the project's own methods
  (`docs/research/10-engine-empirical.md` §8), so a Python package, a Cargo
  workspace and a compilation database are whole up to the next directory that
  declares itself and no further. A trigger inside a dependency directory
  (`node_modules`, `vendor`, `third_party`, `bower_components`, `Godeps`) is not
  a project and never reaches the rule while `workspace.index_vendor` is false,
  which is the default, because those directories are then excluded from the
  snapshot; with it enabled the operator has asked for them to be indexed and
  their manifests are projects like any other. Two things qualify that: the Git
  ignore hook excludes an ignored directory whatever `index_vendor` says, and a
  tracked path wins over both exclusions (Section 10.2) — though detection sees
  a tracked manifest under an excluded directory only when the traversal policy
  it is handed carries the capture's force-include hooks. **A nested project is
  excluded from the one that encloses it**: no profile's argument array can
  exclude a subtree, so the outer indexer still runs over it, and the importer
  drops every document under a nested project root of the same kind, counted
  under `documents_in_other_projects` without degrading anything, because the
  nested unit publishes those paths. A document a unit reaches outside its own
  directory through `../` is admitted only through the project that owns it:
  it is left to the innermost other project of the same kind that holds it,
  the workspace root's included. When no project holds it, no unit publishes
  it — two sibling units reaching one undeclared directory would otherwise
  both admit its files — so it is refused, counted under
  `documents_in_no_project` with its path under
  `documents_in_no_project_exemplar`, and the capability is degraded with
  `CTX_PROVIDER_OUTPUT_INVALID`: the file is still served, with no precise
  facts. One path is published by exactly one unit.
  A project whose scope key
  does not fit the identity bound is refused rather than truncated, because two
  deep directories with a long common prefix cut to the same key and one
  project's facts would be attributed to the other; its files stay with the
  unit that encloses it, and the detection names the refusal under
  `unplanned_project_keys`. A profile this
  machine cannot supply at all — no payload for this platform, a corrupt store
  entry, an invalid override — plans no unit, so a missing tool never costs a
  snapshot materialization or a failed unit. A profile whose payload the lock
  *does* pin for this platform but the store has not installed yet **is**
  planned: `scip.New` installs nothing, and the first unit that needs the
  payload fetches it (see "Payload resolution" below).
- `Verify(ctx, view, scopeKey)` decides `UnitBuild.SourceBinding` before the
  unit is opened. `IndexUnit` repeats the check and reports it in its
  capability states, so a unit opened as verified whose index is not is
  visibly `partial` with `CTX_SOURCE_BINDING_UNVERIFIED`, never silently
  exact.
- Declare every eligible snapshot file of the workspace as the unit's
  inputs: a fact may name any file the index describes, and storage refuses
  a fact on an undeclared file. A document naming a path the snapshot does not
  hold is dropped and counted under `documents_not_in_snapshot` with its path
  under `documents_not_in_snapshot_exemplar`; it does not degrade the
  capability (see "Rejected and superseded documents").
- `Import(ctx, req, sink, opts)` is `IndexUnit` plus the refresh: `opts.Previous`
  is the sealed unit's stored `DocumentManifest` (nil is a full import) and the
  `Report` carries the delta and the fresh manifest; every count of what the
  import did not admit is on the result's capability rows. `IndexUnit` is `Import` with no previous manifest, which
  is all the `provider.Provider` interface can express.

### A refresh without `--scip-index`

The supplied index is an input of the **composition**, not of the request: it
reaches this provider through the workspace open, so a run that does not pass
`--scip-index` constructs a provider with no import path at all. `Detect` then
declares no import input and `Scopes` returns no `import:` key, so the plan
carries no import unit and the generation it publishes holds none.

The honest answer to "does a refresh without `--scip-index` invalidate the
imported units?" is therefore **no, and something more inconvenient than yes**:
the sealed unit is not invalidated, deleted or marked stale — it stays in the
store and is reused verbatim by a later run that supplies the same index with
the same inputs — but it is **not a member of the new generation**, so the
symbols it contributed stop being answerable the moment that generation becomes
active. The capability is not reported as degraded either, because from the
plan's point of view nothing was requested and nothing failed.

Two operational consequences follow. Pass `--scip-index` on **every** run that
should keep the imported symbols, including plain `codectx refresh`. And if
cross-file symbols disappear after a refresh, run `codectx doctor`: its
`supplied_index` check reports what the active generation was actually built
with, which is the one place the difference between "no index was supplied" and
"a supplied path matched nothing" is visible.

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
   before any fact exists (the binding decides which diagnostic code a
   refused coordinate carries).
2. **Definitions**: occurrences and symbols are spooled to a private scratch
   SQLite file under the work directory (bounded by `MaxSpoolBytes`, 4 MiB
   page cache) as they stream; when a document ends its position encoding is
   known, so its definitions are converted, resolved, published, aliased and
   recorded in the on-disk symbol map.
3. **References**: read back from the spool with the map complete, so a
   forward or external reference binds to the identity its definition
   minted. Edges are grouped on disk by relation identity and published with
   one evidence row per distinct occurrence range.

The spool is a surface of the shared scratch pool
([storage](storage.md#the-scratch-pool)), so a second import of the same shape
writes over the first one's bytes and frees nothing; the scratch directory
around it is removed on every path.

## Delta import

A managed indexer always re-indexes its whole unit — only `scip-java` has a
per-file pipeline, and narrowing `scip-typescript` or `scip-python` changes
symbol names — but the importer never rewrites the whole unit. SCIP documents
are independent and global symbol strings are position-free, so a refreshed
unit is applied as a per-document delta (Section 11.4, "Delta import";
`docs/research/12-incremental-scip-lsp.md` Sections 3.3–3.5 and 6.2).

**Document manifest.** `DocumentManifest` is the sorted `(root-relative path,
canonical document hash, refused occurrences)` list of one sealed unit, held as a file rather than a
Go slice because a unit may describe up to `MaxDocuments` documents and Section
6 forbids a whole-repository list in the heap. Its format is

```
codectx-scip-documents v1
<64-hex canonical document hash>  <refused occurrences>  <root-relative path>
...
```

sorted by path with strictly increasing paths. The refusal count is the number
of the document's occurrences its import refused (see "Positions and
binding"); a refresh that carries an unchanged document's rows folds that count
into `refused_occurrences` and degrades the capability exactly as the original
import did. It is exact rather than an estimate: a refusal depends only on the
document's occurrences, symbols, resolved encoding and pinned bytes, which the
canonical document hash covers, so an equal hash refuses the same occurrences.
When every refusal of a run is carried, `refused_occurrence_exemplar` names the
first such document instead of a coordinate. `Provider.Version` moves with
the hash domain, so a manifest written under an earlier mapping is never
reachable: the old unit has a different `UnitID` and is not reused.
`LoadDocumentManifest` validates all of that and refuses a malformed file
rather than treating it as an empty previous state, because an empty previous
state silently imports everything.
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

**Rejected and superseded documents.** A project unit's `relative_path` is
joined to the project's directory and cleaned first, so a document the project
reaches through `../` (`app/../shared/x.ts`) is named by its workspace path
(`shared/x.ts`), and admitted only by the unit of the project that owns it; a held
path no project owns is refused under `documents_in_no_project` (see "One unit per triggering
directory" above). A document whose path still escapes the workspace root after that is rejected
and counted under `documents_outside_root`: 18 of the 141 documents the Go profile's indexer emits
for this repository are the test mains it writes
under `$GOCACHE`, whose paths are `../../../../..`-style escapes into a
content-addressed build cache. Admitting them would bake absolute machine paths
into the index, churn about 13% of the document set for unrelated reasons, and
produce paths that can never join a repository `FileID`. `scip.proto` calls
`relative_path` unique but defines nothing for a duplicate, and its own two
reference consumers disagree (`FlattenDocuments` unions, `expt-convert` keeps
the first), so this importer picks one rule and counts it under
`documents_duplicate_path`: the last document of a path wins and the earlier ones
are dropped. Each count names its first document under `<count key>_exemplar`.
Whether a drop degrades the capability depends on whether it loses a fact about
source this product serves. Every eligible file is in the snapshot, so an
outside-root document and one naming a path the snapshot does not hold
(`documents_not_in_snapshot`) describe no served bytes: they are counted and do
not degrade, since degrading would make every Go unit with tests read partial
over the synthesized test mains. A duplicate path, a document whose position
encoding is neither declared nor measured (`documents_unspecified_encoding`),
and one over `max_source_file_bytes` (`documents_over_source_bound`) each drop
facts about a held file, so they degrade the capability, as a held path no
project owns does: `CTX_PROVIDER_OUTPUT_INVALID` for all but the bound,
`CTX_RESOURCE_LIMIT` for the bound.

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
an identifier the source spells literally, and whose range selects any bytes,
must select exactly that identifier. Namespace descriptors (package, module and
file paths), meta descriptors (Python's `__init__`), backtick-escaped names
(`<init>`, operators) and `local` symbols carry no such name, and a zero-width
range selects no bytes and reads the same in every encoding; all of them are
passed over, however many there are. On a line with a non-ASCII rune before the
token the readings disagree and a wrong guess is caught; on an ASCII-only line
every reading converts to the same bytes, so there is nothing to catch. A
document whose guess the bytes contradict, and one none of whose definitions
can check it, are both skipped rather than admitted on an unchecked guess, and
the capabilities are `partial` with `CTX_PROVIDER_OUTPUT_INVALID`.

The probe is not the guarantee, only its precondition. **Every** occurrence's
range is then proved against the bytes it claims to describe — every one to the
same two checks, whatever it is, and with what those checks cannot catch stated
below rather than left implied. A wrong column does not need a wrong encoding: on a line carrying a tab after other
characters an indexer can count columns to a different tab stop than the file
does, so the range is shifted a few columns, stays inside its line, converts to
a valid rune-aligned extent, and names source that is not the symbol. Measured
on one Java project: 4,714 of 93,167 occurrences, in 91 of its 223 documents —
3,531 refused for starting inside an identifier token and 1,183 for a column
past the end of its line, 4,408 of them on lines indented with spaces followed
by a tab and the remaining 306 on other lines, sampled and found to be the same
shift. None of them is a false positive of the predicate: an identifier holding
a `$` and one holding a non-ASCII letter are both admitted, by construction.

The per-occurrence proof holds **every** occurrence, declaration and reference
alike, to two checks over the pinned bytes.

**Whole-token coverage.** No range may cut an identifier token: an identifier
byte inside the range with another immediately outside it is half a token, and
no grammar produces one. A range that names an identifier on a single line must
in addition begin and end on the tokens it covers rather than on the whitespace
between them — `browser` shifted seven columns left is `return `, an identifier
plus the gap before the next token, which is not how the source spells
anything. Two shapes of range do not name an identifier on a line and keep the
cut check alone: a range spanning lines is a block span, measured, a crate's
whole file; and a range holding no identifier byte at all is punctuation the
grammar spells without one, measured, the reference from `+` to the `add`
method it desugars to, which one indexer ranges over the space beside the
operator. A range of whitespace alone holds no token at all — a reference
shifted onto the gap between two tokens — and is refused. A zero-width range
selects no bytes, so there are none to contradict; measured, every one is a
document-level symbol anchored at the start of a file. It is admitted as an
occurrence, but it proves nothing about an encoding, so the encoding probe
passes over it.

**The name check.** A range that is exactly one identifier token, whose symbol's
last descriptor is a name the grammar spells literally, must select that name or
an identifier the document itself spells for that symbol. A document that
aliases an import spells the symbol under a name of its own — `use HashSet as
Set` makes every later `Set` of that file a correct reference to `HashSet` — and
the occurrence carries nothing saying so: the indexer that produces this shape
sets no occurrence role at all, so the import role cannot mark it. A spelling is
therefore bound only from the **alias clause** that introduces it, spelled
`<name> as <spelling>` on one line with `<name>` the symbol's own name: either
one occurrence of the symbol ranges over the whole clause, or one ranges exactly
over the spelling and another exactly over the name the clause aliases. That is
the one source a shifted column cannot produce. Recurrence is not: a tab counted
to the wrong stop shifts every line of the same indentation by the same distance,
so two identical lines `\t\trun(page,name);` carry the same wrong spelling twice,
and a spelling bound because it recurs would publish `page` over the bytes of
`name`. A uniform shift moves the two occurrences of a clause by the same
distance, so it cannot leave one on the aliased name and the other on the alias;
a cast such as `len as u32` holds only its first token as an occurrence of the
symbol, so a shift onto the type binds nothing. The clause occurrence of an
alias declared and never used binds itself, so it is not refused. An alias form
spelled without `as` (`{A => B}`) binds nothing, and its uses are refused rather
than admitted on a guess.
A range that is not one identifier token is not name-checked at all: an aliased
import can put the occurrence on the whole alias clause (`OrderedDict as OD`),
and an operator reference is punctuation.

Measured over the indexes the six pinned indexers produce from the fixtures of
the per-platform matrix — 308 occurrences, all nine languages — the proof
refuses none of the 306 that spell their symbol's own name or are not
name-checked. The other two are `Set` for `HashSet`, the alias clause and its
use, and the clause admits both. Measured with the pinned build on the
fixture's `use std::collections::{HashMap, HashSet as Set};`, the indexer puts
two occurrences of the `HashSet` symbol on that line, neither with a role: one
exactly on the clause's `HashSet` token (columns 32–39) and one exactly on
`Set` (columns 43–46). That is the second clause shape, so `Set` is bound, the clause's own `Set` is admitted and so is
the later `Set<u8>`. The whole-clause shape is the one the Python indexer
emits: its occurrence ranges over `OrderedDict as OD`, and measured with a use
of `OD` added to a copy of the fixture, the use is an occurrence of the `OrderedDict`
symbol spelled `OD`, which that clause binds.

**What the proof does not catch.** It compares bytes, and it never adjusts or
guesses a coordinate, so a shift that lands on bytes it cannot distinguish from
the truth publishes. Measured by shifting every one of the 308 occurrences by
the two column distances observed in the field: of 214 occurrences a +5 shift
converts at all, 32 still pass, and of 126 a +12 shift converts, 19 still pass —
against 63 and 45 under a rule that checks only where a range starts. Those survivors,
by kind: **24** whose symbol carries no name the grammar spells literally (a
`local` symbol, a namespace, a meta descriptor, a backtick-escaped name), so
there is nothing to compare the token against; **17** whose range is punctuation
rather than one identifier token; and **10** zero-width ranges, which name a
position and no bytes. A shift landing on another token spelling the **same**
identifier is the remaining kind — two byte-identical ranges are the same claim,
and nothing in the bytes separates them; it occurs **0** times in this corpus
under those two shifts. The alias clause adds one narrower kind: a line whose
occurrences carry two *different* shifts (a tab in the middle of the line as
well as in its indentation) could in principle place one occurrence on a
clause's aliased name and another on its alias, which a single shift cannot.

The two proofs deliberately have different outcomes, because the two failures
have different reach. A failed **encoding probe** is a claim about the whole
document — every column of it is read in an encoding its bytes contradict — so
the document is dropped whole, counted under `documents_dropped_encoding` and
named by `documents_dropped_encoding_exemplar`. A document none of whose
definitions can check the guess is dropped whole too, because nothing confirmed
it, but nothing contradicted it either, so it is counted apart under
`documents_unproved_encoding` and named by
`documents_unproved_encoding_exemplar`. A failed **per-occurrence
proof** reaches exactly one coordinate, so exactly one occurrence is left out:
it is counted under `refused_occurrences`, the first is named with its reason by
`refused_occurrence_exemplar` as `<document>:<line>:<column>: <reason>` — the
coordinate the occurrence claimed, spelled as the index spelled it, zero-based
and with the column counted in the position encoding the document declared,
which is the encoding the refusal is a disagreement about, so that an operator
opens the disagreeing line instead of re-running the indexer to find it — and
the unit publishes `partial` with
`CTX_PROVIDER_OUTPUT_INVALID` rather than failing. Under an unverified binding
the index describes bytes it never saw, and the same coordinate is refused the
same way: it is counted under `refused_occurrences` and can be the
`refused_occurrence_exemplar`, and the unit keeps the
`CTX_SOURCE_BINDING_UNVERIFIED` it already carries, because an unverified
binding outranks every later reason.

A unit is never failed over one occurrence. Failing closed on the first refusal
threw away all 93,167 occurrences of the project above over 4,714 wrong columns:
every fact the indexer got right was lost with the ones it got wrong, and an
operator was told the unit was unavailable rather than thinner. The counts are
what makes the thinner unit honest — a partial capability that does not say how
much it left out is indistinguishable from a whole one.

`Metadata.text_document_encoding` is deliberately never consulted. All six
indexers set it to `UTF8`, including the three whose columns are UTF-16,
because `scip.proto` defines it as the encoding of the source files on disk and
says it is unrelated to ranges. Reading it as a position encoding would convert
every UTF-16 column as a byte offset.

A document of any other tool build that does not declare an encoding is
skipped and the capabilities are `partial` with `CTX_PROVIDER_OUTPUT_INVALID`:
Section 9.3 forbids guessing one.

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

## Identity and alias scopes

Every node goes through `req.Resolver`; the provider copies
`Resolution.CanonicalKey` verbatim.

| Symbol | Alias scope key | Candidate |
|---|---|---|
| `local N` | `file:<path>` | located (file + definition range), no qualified name |
| global | `pkg:<manager> <package-name> <version>` from the symbol's package descriptor | located; qualified name = the raw descriptor string (`pkg/Foo().`) |
| symbol never defined in the index (external) | as above | unlocated: structural key over scope, qualified name, kind; metadata `"scip_external":true`; evidence carries no file and no range, because the entity has no source location in this index and Section 9.3 requires an absent range rather than a stand-in. The referencing location is on the reference edge |
| the file node a file-level occurrence points out of | `workspace`, native key `file:<path>` | exactly the filesystem provider's file candidate: kind `file`, name `path.Base`, qualified name = path, no defining file, no range, no language. Resolving it against the declared `filesystem` dependency adopts that provider's identity instead of minting a second file node for one path; SCIP's own evidence row (`[0, size)` of the pinned file) stays on it |

Name is `display_name`, else the last descriptor's name. Signature is
`signature_documentation.text`, truncated to the signature ceiling when it is
longer and flagged on the node as `"truncated_fields":{"signature":<original
byte length>}` — the same index-time truncation attribute the structural
provider publishes, never merged with a result page's transient truncation
flag. Only the ceiling is ever read off the wire, so a signature of any size
costs the ceiling and not itself. Ambiguous resolutions become `may_refer_to`
edges.

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
row. One relation carries at most `model.MaxEvidencePerFact` (65536, or the user-set `index.max_evidence_per_fact`) evidence
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
exceed `model.MaxScopeKeyBytes` is skipped, counted under
`callsite_aliases_skipped` and degrades the capability with
`CTX_RESOURCE_LIMIT`, never truncated into a key that would join the
wrong range.

## Bounds

`scip.Limits` is seven `providers.scip.*` keys, **all unlimited by default**,
and one constant. Nothing here refuses a repository for its size unless an
operator asked for it: `max_index_bytes`, `max_manifest_bytes`,
`max_documents`, `max_occurrences_per_document` and `max_spool_bytes` cut
nothing at all, because the index streams record by record, documents and
occurrences spool to an on-disk database and a manifest is scanned line by
line. A value an operator sets and this run crossed is published on every
capability row of the unit as `partial` with `CTX_RESOURCE_LIMIT`, under the
detail `resource_limits_exceeded`, as a sorted `key=seen/bound` list — the run
reports that the figure was passed and admits every fact regardless.

Three of the seven do leave something out: `max_source_file_bytes` (a document
whose source would be held whole is skipped, reported under the same detail),
`max_materialize_bytes` (files left out of an indexer's private copy, named in
the materializer's own operator report with a complete count) and, for C and
C++ only, `max_manifest_bytes` (a compilation database over it is left
un-normalized, which costs that unit its whole run — reported, never silent).
Those three are therefore part of the index fingerprint; the four pure
reporting thresholds deliberately are not, so adjusting one never invalidates
an index.

`MaxRecordBytes` (4 MiB) is the one bound that stays product code. It is the
wire reader's pre-allocation ceiling — the bytes one record may cause to be
allocated before it is decoded — not a figure about the repository, and it
must stay positive.

The record buffer — the only buffer sized by untrusted index bytes — is
charged against the sink's byte pool through `Reserve` when the sink offers
it. One document's source bytes are **not** charged: `BatchSink.Reserve`
refuses a reservation above `resources.max_provider_record_bytes` (4 MiB),
while a source file is bounded only by what the operator set in
`max_source_file_bytes`, so the charge is impossible for exactly the largest
case. At most one document's source is held at a time, so a unit retains one
document's source outside the pool's accounting, on top of the pool's own
budget — the one place an unset bound leaves peak memory a function of the
largest file the index describes, which is why this bound exists to be set.
That buffer is sized by the pinned snapshot file, whose size is checked before
the read.

## Payload resolution

`scip.New` **installs nothing**. It asks the toolchain what the store already
holds (`toolchain.ResolveInstalled`, which is `Resolve` with the fetch
refused), and sorts the six kinds into three states: ready, deferred (pinned
for this platform, not installed) and unavailable (a typed `CTX_TOOL_*`
reason). Resolving with a fetch here installed every language's indexer on a
machine that had not run `codectx tools prefetch`, before any detection had
happened and before any unit existed — 24.8 s of fetching at construction,
against 50 µs and zero files written now.

A deferred payload is fetched by the first unit that runs it, at the one moment
the repository is known to contain the language; a fetch that fails fails that
unit with the toolchain's typed code.

`Descriptor().Version` is fixed at construction — `Descriptor()` takes no
context and has to be a deterministic function of the process — and folds the
identity of every kind that can produce facts: the resolved fingerprint of a
payload the store holds, and `toolchain.Resolver.PinnedFingerprint` for a
deferred one, which computes the same fingerprint from the lock alone. The two
are the same string for the same payload, so a unit sealed by the run that
fetched the payload keys identically to every unit after it. Folding an empty
slot for a deferred kind instead would key the first run on a cold machine
under a provider version that names no tool at all, and the next process would
re-index everything that run produced. Only a kind this machine cannot supply
at all contributes an empty slot, and it plans no unit and produces no facts.

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
result of each run were recorded when the matrix was verified. The six
profiles and the argument arrays this build pins, after the payload's own
launcher prefix:

| Profile | Triggers | Arguments after the launcher | Environment allowlist | Network posture | Host toolchain it needs |
|---|---|---|---|---|---|
| `scip-go` | `go.mod`, `go.work` | `index --output <output>` | `PATH HOME GOPATH GOCACHE GOMODCACHE GOFLAGS GOPROXY GOPRIVATE` | allowed | `go` |
| `scip-typescript` | `tsconfig.json`, `jsconfig.json`, `package.json` | `index --cwd <input> --output <output> --no-progress-bar --infer-tsconfig` | `HOME` | denied | none (managed Node) |
| `scip-python` | `pyproject.toml`, `setup.py`, `setup.cfg`, `requirements.txt` | `index --cwd <input> --output <output> --project-version 0.0.0 --quiet` | `PATH HOME` | denied | `python3`, `pip3` |
| `scip-java` | `pom.xml`, `build.gradle`, `build.gradle.kts` | `index --scip-config <input>/scip-java.json --targetroot <work>/scip-java-targetroot --output <output>` | `PATH HOME` | denied | none (managed JDK) |
| `rust-analyzer` | `Cargo.toml` | `scip <input> --output <output>` | `PATH HOME CARGO_HOME RUSTUP_HOME` | allowed | `cargo` |
| `scip-clang` | `compile_commands.json`, `compile_flags.txt` | `--compdb-path=<input>/compile_commands.json --index-output-path=<output>` | `PATH HOME` | denied | none (carries its own Clang); needs the compilation database the project's build produced |

**The host-toolchain column is not optional.** Three of the six load the
project's model through that language's own toolchain, the way any build does,
and cannot do otherwise: `scip-go` drives go/packages, `scip-python` reads the
project's installed distributions through the interpreter and pip, and
`rust-analyzer` loads `cargo metadata`. Where the toolchain is absent, that
language has no precise index — the profile exits non-zero and the unit fails
with `CTX_PROVIDER_UNAVAILABLE` — and the structural and dependence providers
still cover it. `scip-java` is not in that list: it compiles the snapshot with
the managed JDK's own `javac` and needs no host build tool (see below).

`<input>` is the unit's project directory inside the private materialization,
which is also the child's working directory; `<output>` and `<work>` are under
the run directory, outside the materialization.

**A profile's private copy is the whole workspace, deliberately — and the
indexer is run over one project inside it.** The two are different decisions.
The materialization is not narrowed to the files a unit "owns": a precise
indexer resolves symbols through the project's own dependency context — the
module graph, the package manifests, the installed distributions, the headers,
a `tsconfig` above it — so a narrower copy would produce an index that
describes less than the unit claims. What the indexer is *run over* is the
unit's project, which is what makes the unit a project rather than a
repository. The call site says so explicitly rather than leaving it to an
omitted selection.

What the copy cannot supply is a dependency directory: those are excluded from
the snapshot, so an indexer that resolves installed packages by reading them
out of the tree it is given resolves them to nothing. That is a limit on what a
unit publishes — the project's own definitions and the references among them —
not a reason for the unit to fail.

**Document paths are prefixed back to the workspace.** An indexer writes
document paths relative to what it was run over: `scip-go` run in `sub/`
writes `a.go`, not `sub/a.go` (measured against the pinned payload). The
import prepends the unit's project directory to every document path once, as
the document is decoded, so every path the import resolves, stores and
publishes is workspace-relative whatever project the unit is. Without it every
document would fail to resolve against the snapshot and the unit would trip
the false-readiness refusal below. The launcher prefix comes from
the lock: a self-contained binary runs as itself, a Node-hosted indexer runs
as `<managed node> <entry>`, and `scip-java`'s launcher runs with `JAVA_HOME`
pointing at the managed JDK. The child's environment is exactly the
allowlisted variables the parent has plus the variables the payload needs; the
payload's come last, so a host `JAVA_HOME` can never shadow the pinned
runtime.

Everything is executed through the shared `internal/process` runner with an
argv array only: no shell anywhere. The run is bounded by the
smaller of the profile's own timeout and `providers.scip.timeout`, by the
profile's memory and disk reservations, and by 1 MiB of captured output per
stream.

**`scip-java` compiles the snapshot itself.** Its default mode drives the
project's own Maven or Gradle build, found on the host `PATH`, which resolves
dependencies from a remote index: on a machine with neither, the profile exited
1 and produced nothing (measured). The profile instead writes a `scip-java.json`
into the private materialization — naming the materialization root as both the
source root and the only source directory, with empty `classpath` and
`dependencies` — and passes `--scip-config`, which makes the indexer compile the
sources with the managed JDK's own `javac`. The Java profile therefore needs no
host build tool and resolves nothing remotely, which is why its posture is
`denied` and why `MAVEN_OPTS`/`GRADLE_USER_HOME` are not in its allowlist.
Measured on `linux/amd64` with `PATH=/usr/bin:/bin` (no Maven, no Gradle): the
unit seals with all three capabilities fresh. A repository's own
`scip-java.json` is replaced in the copy: the invocation is product code, not a
configuration surface. `--targetroot` keeps the indexer's own intermediate
output inside the run directory rather than in the materialization.

A materialization that holds **no** source file the indexer can describe is
refused before that indexer starts, with `CTX_PROVIDER_OUTPUT_INVALID` and a
remediation naming the source-free case. Two profiles apply it, through one
helper. For Java it is the aggregator POM of a multi-module repository —
`pom.xml` triggers the profile, the root carries no source of its own — and
without the check the compiler refuses, the indexer exits 1, and the run fails
as `CTX_PROVIDER_UNAVAILABLE` with an exit status (measured). The walk does not
descend into a nested project of the same kind — a module directory holding its
own `pom.xml` — because that module is its own unit and its documents are
dropped from the aggregator's, so the modules' sources cannot answer for the
aggregator.
For TypeScript it is a package directory holding only a manifest, a lock file
and documentation, with no `.ts`/`.js` beside them: measured on a real
repository, the run failed as `CTX_PROVIDER_UNAVAILABLE: node exited with
status 1`. Both are a process failure with no stderr and no remediation, for a
condition this provider can name precisely. `node_modules` is excluded from the
TypeScript walk: a dependency's own sources are not the project's, and counting
them would restore that opaque failure with an extra step. The refusal is the
same typed one the other profiles produce for an index that describes no
admitted document, raised before a tool is started rather than after.

**Three pinned arguments exist because of a measured failure, not a
preference.** `scip-typescript` is given `--infer-tsconfig` because two of its
three triggers — `jsconfig.json` and `package.json` — name a project that has
no `tsconfig.json`. Without the flag the indexer prints `(missing
tsconfig.json)`, indexes nothing and exits 1, so every such project fails its
unit with `CTX_PROVIDER_UNAVAILABLE` and zero records: nine projects of one
monorepo failed exactly that way, and every one of them indexes with the flag.
With it the indexer infers the configuration it needs and writes it into the
private materialization — never into the repository, which this provider never
writes to — and indexes the project. A project that already carries a
`tsconfig.json` is unaffected: the flag is consulted only when the file is
absent, so a configured project is still indexed under its own configuration.
The symbols such a unit can resolve are bounded by the paragraph above: a
snapshot holds no dependency directories, so a name owned by an installed
package has no definition in the index, and what the unit publishes is the
project's own definitions and the references among them.
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
removed on success, failure and cancellation. Those removals return at once --
the tree is renamed into the process's to-free set -- and the space is given
back off the run's path, a window at a time
([storage](storage.md#what-a-run-still-frees)). An indexer's tree is copies
rather than links to the content store because the profile writes into the tree
it was given: `scip-clang` normalizes its compile database there and
`scip-java` writes its project configuration there, and a link would put those
writes through to the published blob.
