# Filesystem, documentation and manifest providers

`internal/provider/filesystem` and `internal/provider/manifest` are the two
base providers of Section 11.2. Together they publish every repository,
directory, file, document, package, module, dependency, configuration and
build-target node of the base index, the `contains`, `defines`,
`depends_on`, `builds`, `configures` and `documents` relations between them,
and the lexical search documents every other search feature builds on. Both
follow the provider contract of [providers.md](providers.md): facts flow only
through the sink, identities come only from the resolver, bytes come only
from the pinned snapshot view.

## Units

A unit is one file for both providers. The coordinator plans:

| Provider | One unit per | Scope key | Inputs | Dependencies |
|---|---|---|---|---|
| `filesystem` | every non-deleted snapshot file | `filesystem.ScopeKey(path)` = `"file:"+path` | that file | none |
| `manifest` | every file with `manifest.Recognize(path)` true | the same scope key | that file, and for a requirements file every file it includes | the `filesystem` unit of the same file |

The unit's `InputHash` is the digest of its input file versions, so an
unchanged file reuses its sealed unit with zero work, and a change to one
file invalidates its two units. A requirements file is the one manifest that
reads other files: the planner resolves its `-r` and `-c` includes,
transitively, against the same snapshot view and under the same
`max_parse_file_bytes` the unit reads them with (`manifest.Includes`), and
declares each included file the snapshot holds as an input of its unit. An
edit to an included file therefore rebuilds every requirements unit that
includes it, directly or through another include. An include that names no
file in the workspace (a URL, an absolute path, an environment variable, a
path outside the workspace) or one the snapshot does not hold is not an
input; it is reported on the unit as described under "Requirements files".
An included file over `max_parse_file_bytes` is an input, since its size is
in the unit's report, but the files it includes are not followed. An include
cycle ends, with each file of it an input once. `filesystem.PathFromScope` recovers the
path inside `IndexUnit`; a scope that is not a normalized file scope is a
coordinator error.

Both providers are `Required` and always detect as available. `manifest`
detection additionally reports the well-known root manifests it finds
(`go.mod`, `go.work`, `package.json`, `composer.json`, `Cargo.toml`,
`pyproject.toml`, `requirements.txt`, `pom.xml`) and lock files (the names
under "Lock files") through `Root.Lstat`; nothing is walked.

## Identities and aliases

Every node is minted through `req.Resolver`; the providers never derive a
canonical key. The candidate shapes decide the Section 9.4 basis:

| Node | Candidate | Basis | Native alias (scope `workspace`) |
|---|---|---|---|
| repository | qualified name `.` | structural key | `repo:` |
| directory | qualified name = path | structural key | `dir:`+path |
| file | qualified name = path, language from path | structural key | `file:`+path |
| document / configuration / build_target a path is recognized as | qualified name = path | structural key | `<kind>:`+path |
| module / package / configuration a manifest defines | qualified name = ecosystem-qualified name, `FileID` = the manifest | qualified signature | `<kind>:`+qualified name |
| dependency | qualified name = ecosystem-qualified name, no file | structural key | `dependency:`+qualified name |

Path-identified nodes carry no `FileID` in their candidate: identity is a
pure function of the path, so every unit that mentions a path
mints the same node and storage deduplicates the identity. That is what lets
a file unit publish its ancestor directories as idempotent facts, a go.work
unit name the directories it uses, and a Markdown unit link to another
file, all without any unit reading beyond its own input. The owning unit's
fact carries the file, content hash and size as attributes; a referencing
unit's fact carries `{"resolution":"referenced"}` so a consumer can tell an
observed file from a merely named one. A package or module is qualified by
its defining file because two manifests declaring the same name are two
packages; a dependency is workspace-wide because two manifests naming the
same ecosystem package mean the same thing.

Qualified-name forms: `go:<module path>`, `npm:<name>`, `cargo:<crate>`,
`pypi:<PEP 503 normalized name>`, `maven:<groupId>:<artifactId>`,
`go:work:<path>` and `cargo:workspace:<path>` for workspace configurations.

Dependent providers resolve these nodes either by the alias (basis
`native_key`) or by building the same candidate with
`filesystem.PathCandidate(providerID, kind, path)`; both yield the same
identity.

## What `filesystem` emits per file

- repository, each ancestor directory and the file node, with `contains`
  edges down the chain; the file node's metadata is
  `{"size","executable","binary","format"}`;
- for a path `Classify` recognizes, the `document`, `configuration` or
  `build_target` it defines and a `defines` edge from the file, at precision
  `heuristic` (recognition is by path, not by parsing), plus a name-only
  search document for it;
- for a README-style document, `documents` from the document to its
  containing directory (or the repository);
- the file's lexical search chunks (below).

`filesystem.Classify` is the single path classification table. The language
half of it lives in `internal/lang`: `lang.Of(path)` is the one place a path
becomes a language tag, so the tag a snapshot records in its manifest and the
tag a provider selects files by cannot drift apart. Its table covers the source
languages and the documentation, build and configuration extensions.
`filesystem.Language(path)` is a thin call to it and stays the name the
analyzer providers call; the tags for the bundled grammars match
`tree_sitter.languages`. A `.h` header, which the C and C++ grammars both
declare, is tagged within its snapshot (`lang.For`): the grammar the snapshot's
C and C++ translation-unit census parses headers with first. The manifest row,
the search language filter and the file node all carry that tag.

Recognition-only nodes (a Dockerfile, a Terraform file, an OpenAPI
description) are emitted by `filesystem`, not by `manifest`: classification is
a pure function of the path that the file's own unit already evaluates, so no
manifest unit needs to be scheduled just to publish one node. `manifest`
handles only the formats that require parsing.

Recognition-only formats are Dockerfile and compose files, GitHub Actions,
GitLab CI, CircleCI, Jenkins, Travis, Azure Pipelines and Bitbucket
Pipelines, Terraform, kustomize and Helm charts, Gradle, Bazel, CMake and
Make, OpenAPI, GraphQL, protobuf and SQL, and AsciiDoc, reStructuredText,
plain text and ADRs (Markdown under an `adr` directory). They receive a
classified node and nothing else: no build, template or schema evaluation is
inferred. Unknown manifests are ordinary files and stay searchable through
their chunks.

Derived counts (files per directory, dependencies per package) are never
stored on nodes; they are computed from the selected generation.

### Search chunks and admission

Chunks are stored once, in the file's own unit, and are the only place a
source body is copied into the index; symbol documents carry names only.
Chunking uses the shared `source.PlanChunk`: a chunk is at most 32 KiB
(`model.MaxSearchBodyBytes`), ends on a line boundary when one fits, and a
line longer than a chunk is split at a legal boundary, never inside a
well-formed UTF-8 sequence. Consecutive chunks
overlap by at most two whole lines and at most 1 KiB, so a pathological line
cannot make the overlap a second copy of the chunk; a chunk that ends inside
a split line has no overlap. The window held in memory is one chunk.

A chunk's search key is `H("search-chunk-v1", file, content hash, start,
end)`; its body is exactly the bytes of `[start,end)`.

`model.SearchUnit.Bytes` is a required value field, so a name-only document
whose node the parser could not place carries the empty range `[0,0)`. Read
`[0,0)` as "this document has no byte range", never as the first zero bytes of
the file; a document that really covers bytes always has `End > Start`.

Admission affects declared coverage only, never CAS retention or source
serving. The `search` capability is reported per file:

| Condition | State | Diagnostic |
|---|---|---|
| size over `workspace.max_search_file_bytes` | `unavailable` | `CTX_RESOURCE_LIMIT` |
| over 30% of the first 8000 bytes is not text, or they carry a run of 4 NULs | `unavailable` | `CTX_BINARY_CONTENT` |
| some chunks were not UTF-8 text (skipped; source reads serve them as base64) | `partial` | `CTX_ARGUMENT_INVALID` |
| otherwise | `fresh` | |

The `structure` capability is always `fresh` for a unit that seals.

`CTX_BINARY_CONTENT` is its own Section 22 family rather than a reuse of
`CTX_PROVIDER_UNAVAILABLE`: the provider ran and answered, and the answer is
that the file is not text, so a lexical index has nothing to say about it.
Reporting it as an unavailable provider would send an operator looking for a
broken installation that does not exist.

## What `manifest` emits per file

The manifest unit resolves the file node (an alias hit on its filesystem
dependency) and emits `defines` from it to the node the file declares, a
name-only search document per declared or depended-on node, and the
relations below. Each dependency occurrence is one evidence row on the
canonical `depends_on` edge whose `detail` is a JSON object carrying the
dependency `kind` (`runtime`, `dev`, `peer`, `optional`, `build`, `test`), the
`requirement` as written, and format-specific keys. A dependency a manifest
lists under two kinds keeps both occurrences; deduplication is of the edge,
never of the kinds.

| Format | Parser | Defines | Dependencies and relations |
|---|---|---|---|
| `go.mod` | `x/mod/modfile.Parse` (strict: a repository go.mod is a main module, and lax parsing drops `replace`/`exclude`) | `module` with `go`/`toolchain` metadata | `require` → `depends_on` kind `runtime` with `indirect`; `replace` → `configures` with `replace` target; `exclude` → `configures` with `exclude` |
| `go.work` | `modfile.ParseWork` | `configuration` | `use` → `configures` the directory (the repository for `use .`); `replace` as above |
| `package.json` | `encoding/json`; ranges from one token walk | `package`, metadata `version`, `private`, `workspaces` (globs unexpanded), `scripts` (never executed) | `dependencies`/`devDependencies`/`peerDependencies`/`optionalDependencies` → kinds `runtime`/`dev`/`peer`/`optional` |
| `composer.json` | `encoding/json`; ranges from the same token walk | `package` `composer:<name>` lowercased (an unnamed root project is identified by its path), metadata `version`, `type`; PHP code is not parsed | `require` → `runtime`, `require-dev` → `dev`; the platform entries (`php` and its variants, `hhvm`, `ext-*`, `lib-*`, `composer`, `composer-plugin-api`, `composer-runtime-api`) are dependencies with `platform: true` |
| `Cargo.toml` | BurntSushi/toml; ranges from a bounded line scan | `package` (or `configuration` for a virtual workspace manifest); a `[package]` field written `{ workspace = true }` is listed in `inherited` and not resolved | `[dependencies]`/`[dev-dependencies]`/`[build-dependencies]` and their `[target.*]` forms → `runtime`/`dev`/`build`; `optional = true` → `optional`; `{ workspace = true }` → `inherited: workspace`, requirement unresolved; `[workspace.dependencies]` → `depends_on` with `declared: workspace` |
| `pyproject.toml` | BurntSushi/toml; per-requirement ranges are the quoted string inside the located array | `package`; `dynamic` fields are metadata and never invented | PEP 621 `dependencies` → `runtime`; `optional-dependencies` → `optional` with `extra`; PEP 735 `dependency-groups` → `dev` with `group`; `build-system.requires` → `build`; Poetry tables → `runtime`/`dev` |
| requirements file | line reader of the subset below | `configuration` `pypi:requirements:<path>`; each line this reader does not interpret is one evidence row on that node with its `line` text | requirement lines → `depends_on` kind `runtime`; constraint lines → `configures` with `constraint: true`; each `-r`/`-c` → `configures` the included file with `include: requirement`/`constraint` |
| `pom.xml` | streaming `encoding/xml` tokens with depth (32), element (200k) and text (4 KiB) bounds | `package` `maven:group:artifact`; a missing `groupId`/`version` is taken from `<parent>` and listed in `inherited`; `<properties>` are metadata | `<dependency>` scope `compile`/`runtime` → `runtime`, `test` → `test`, `provided`/`system`/`import` → `build`, `<optional>true` → `optional`; `<parent>` → `depends_on` kind `build`, `role: parent`; `<dependencyManagement>` → `configures` with `managed`; `<modules>` → `builds` the module directory; `${property}` references stay literal with `unresolved: property`; only the direct children of a `<dependency>`/`<parent>` set its coordinates, so a `<exclusions><exclusion><groupId>` describes the exclusion and is not indexed |
| Markdown (and ADRs) | bounded line scanner | the `document` node (idempotent with the filesystem fact), and one `section` node per ATX heading | each heading → a `section` node the document `contains`, named by the heading text and qualified `<path>#<trail>` (the enclosing heading path, joined ` > `), located at the heading line, with its own name-only search document; links and reference definitions whose destination is a workspace path → `documents` to the file or directory, precision `heuristic`; fenced code is skipped; URLs and fragments are not paths |

A heading is its own entity, not a label on the file. Each one gets a
`section` node, so a query that matches thirty headings of one document answers
with thirty hits at thirty ranges instead of folding them onto the document.
Identity is the heading trail rather than a byte offset, so inserting a
paragraph does not re-key the sections below it; a heading repeated verbatim
under the same parent takes an ordinal suffix (`…#Guide > Usage~2`) so the
repeat stays a second section rather than converging on the first.

A Maven child both **inherits its identity from** `<parent>` (a missing
`groupId` or `version` is taken from it and the field is listed in the node's
`inherited` metadata) **and depends on it**: the same `<parent>` element also
produces a `depends_on` edge to `maven:<parent group>:<parent artifact>` with
`kind: build` and `role: parent`. The two are not alternatives — inheritance
is what the child's coordinates are, the edge is the build-time dependency the
POM declares — and both come from the one element, at its byte range. `build`
+ `role: parent` is used rather than an `extends` relation because the
relation set of Section 11.2 is closed.

A `<exclusions>` block is deliberately not indexed: an exclusion is a negative
statement about a transitive graph this provider does not resolve, and
emitting its coordinates as a dependency would be a wrong fact, not a missing
one.

TOML evidence ranges come from a line scan bounded by
`providers.manifest.max_toml_lines`, which is unlimited by default. When an
operator sets it, the scan stops at that line: facts declared before it keep
their exact byte ranges and only the facts past it carry evidence without a
range. The documented degradation is evidence without a range, never a
guessed one.

### Lock files

A manifest declares what a project asks for, often a range; a lock file
records the exact version a resolver selected for every direct and indirect
dependency, and the graph between them. The provider reads these, each by
its tool's file name:

| File | Ecosystem | Reader | Integrity | Dependency graph |
|---|---|---|---|---|
| `package-lock.json` | `npm` | JSON tokens; `packages` (lock versions 2 and 3) one entry per decode, else the version 1 `dependencies` tree | `integrity` | each entry's dependency tables; a dependency resolves as Node does, to the nearest `node_modules/<name>` up the requiring package's path |
| `bun.lock` | `npm` | JSON tokens after comments and trailing commas are blanked in place, so every offset is the file's | the tuple's integrity | the tuple's dependency tables; resolved through the nested install paths (`parent/name`, then each enclosing path, then `name`) |
| `yarn.lock` | `npm` | line reader of both formats (classic `key "value"`, and the YAML one with `__metadata`) | `integrity` (classic), `checksum` (YAML) | each block's dependency tables; a dependency resolves to the block whose descriptor list holds `name@range` |
| `pnpm-lock.yaml` | `npm` | line reader of `packages` and `snapshots` (keys `/name/version`, `/name@version` and `name@version`, peer suffixes dropped) | `resolution.integrity` | `dependencies` and `optionalDependencies` with the exact version each resolves to (in `snapshots` from format 9) |
| `Cargo.lock` | `cargo` | TOML, one `[[package]]` decoded at a time | `checksum` | `dependencies` strings `name`, `name version`, `name version (source)` |
| `poetry.lock`, `pdm.lock`, `uv.lock`, `pylock.toml` / `pylock.<name>.toml` (PEP 751) | `pypi` | TOML, one package table decoded at a time | the hash of the package's one source distribution | `[package.dependencies]` (Poetry), PEP 508 strings (PDM), `dependencies` / `optional-dependencies` / `dev-dependencies` tables (uv), `dependencies` (PEP 751) |
| `Pipfile.lock` | `pypi` | JSON tokens, one package per decode | none (per-file hashes are counted) | none: the format records no graph |
| `gradle.lockfile` | `maven` | line reader of `group:artifact:version=configurations` | none | none |

The lock file defines a `configuration` node `<ecosystem>:lock:<path>`
(metadata `format` and the format's `lock_version`). Each pinned package is
one `depends_on` occurrence from it to the package's `dependency` node, the
same node a manifest's requirement of that name reaches (names normalized
as the manifest readers normalize them: PEP 503 for Python,
`group:artifact` for Maven), with the detail `kind: locked`,
`resolved_version`, `integrity` where the format records one for the
package, `hashes` (the count of distribution-file hashes a Python lock pins),
`source` as the lock writes it, `group` (a Pipfile.lock section, a Poetry or
PDM group, the Gradle configurations), `local: true` for a package of the
repository itself (a workspace member, a path, editable or virtual install)
and `set_size`, the number of versions the lock pins for that name. A name
pinned at several versions publishes one occurrence per version, so the set
is published with its size and never resolved by preference.

Each pinned package then has one `depends_on` occurrence from its
`dependency` node to each dependency it names, with `kind` (`runtime`,
`optional`, `peer`, `dev`), the depending package's `version`, the
`requirement` as the lock records it, and `resolved_version` when the lock
says which version it is: by the format's own lookup, or because the lock
pins that name exactly once.

Resolved versions and integrity are evidence detail, not node metadata: the
dependency node is workspace-wide (every manifest and lock naming the
package reaches it), and a node's metadata belongs to one unit, so a second
lock file pinning another version would replace the first. On evidence,
each lock's statement is kept with its file and range.

Memory: the file's bytes are retained (as for every manifest, under
`workspace.max_parse_file_bytes`) and read as a stream of tokens, lines or
package tables, so the largest value decoded is one package, never the
document. The unit holds one small record per pinned package (name,
version, integrity, dependency names) to count each name's versions before
publishing, which is proportional to the lock, as the unit's own facts are.

Not read: a Python lock's per-file hashes beyond the count, Cargo.lock
version 1's `[metadata]` checksums, and Poetry lock 1.x's
`[metadata.files]`; `npm-shrinkwrap.json` (the package-lock format under
another name), `bun.lockb` (binary) and the per-configuration Gradle lock
files of `gradle/dependency-locks/`.

### Requirements files

A requirements file is a `.txt` or `.in` file whose name contains
`requirements` (`requirements.txt`, `requirements-dev.txt`,
`dev-requirements.in`), or any `.txt` or `.in` file directly inside a
directory named `requirements`. pip accepts any name; a file outside this
set is read only when a recognized one includes it. The supported subset
of pip's requirements file format:

- A line ending in `\` continues on the next line; a comment line ends a
  continuation.
- `#` at the start of a line, or after whitespace, starts a comment to the
  end of the line.
- A requirement line is a PEP 508 requirement. The name, PEP 503 normalized,
  is the dependency's identity; its extras, version specifiers, direct
  reference and environment markers are the `requirement` as written.
  Markers are carried, never evaluated.
- Per-requirement options after the requirement (`--hash`,
  `--config-settings`) are carried verbatim as `options`.
- `-r`/`--requirement` includes a requirements file and `-c`/`--constraint`
  a constraints file, resolved relative to the directory of the file that
  names it. The included file's lines are published on the including file's
  node with `via` naming the file that declares them and without a range,
  since evidence ranges are in the unit's own file. Each file is read once
  as requirements and once at most as constraints, so an include cycle ends.
- A constraint pins a version and installs nothing, so a constraint line is
  `configures` with `constraint: true`, never `depends_on`.
- Everything else is published as written, never interpreted: every other
  option line (`-e`, `--index-url`, `--find-links`, ...), a requirement that
  is a bare URL, path or archive, and an include that is a URL
  (`reason: url`), absolute (`absolute`), outside the workspace
  (`outside_workspace`) or names an environment variable (`environment`).
  Each such line is one evidence row on the file's `configuration` node, at
  precision `syntax`, whose detail is `line` (the text), `reason` (for an
  include) and `via` (for a line of an included file). A line of the unit's
  own file carries its range; a line of an included file carries none, like
  that file's requirements. The node has no metadata list of these lines: a
  node's metadata is one payload bounded at 8 KiB, past which it is replaced
  by `{"truncated":true}`, while its evidence is rows read a page at a time,
  so a file with thousands of option lines keeps every one. A line longer
  than the evidence detail bound (1 KiB) is shortened, and the capability
  is `partial` with `CTX_RESOURCE_LIMIT` and counts such lines under the
  detail `evidence_detail_bytes`; two identical lines of
  the same included file, having no range to tell them apart, are one row.
  A requirement that names an environment variable after its name is
  published with `unresolved: environment`.
- An include the snapshot does not hold, or an `-r`/`-c` with no path
  (`reason: malformed`), leaves the capability `partial` with
  `CTX_ARGUMENT_INVALID`; an included file over
  `workspace.max_parse_file_bytes` leaves it `partial` with
  `CTX_RESOURCE_LIMIT` and the detail `max_parse_file_bytes`.

Every fact carries evidence at precision `syntax` with the exact byte range
of the declaring line, element or token when the parser exposes one;
otherwise the evidence binds to the file without a range rather than
guessing.

### Capability outcomes

The unit reports `manifests` (or `documentation` for Markdown) at its file
scope:

| Outcome | State | Diagnostic |
|---|---|---|
| parsed | `fresh` | |
| a list was cut at a user-set `providers.manifest.max_dependencies` or `providers.manifest.max_entries`, or the parse at a user-set `providers.manifest.max_toml_lines` or `providers.manifest.max_xml_elements` (all four unlimited by default, so this outcome cannot arise from the shipped configuration); the capability names the key and the count that crossed it | `partial` | `CTX_RESOURCE_LIMIT` |
| one entry was unusable (a dependency without a name, a path outside the workspace, a requirements include the snapshot does not hold) | `partial` | `CTX_ARGUMENT_INVALID` |
| the file does not parse as its format | `failed` | `CTX_ARGUMENT_INVALID` |
| size over `workspace.max_parse_file_bytes` | `unavailable` | `CTX_RESOURCE_LIMIT` |

A requirements file adds the two outcomes its own section lists: an
included file over `workspace.max_parse_file_bytes`, and a line shortened to
the evidence detail bound, each `partial` with `CTX_RESOURCE_LIMIT`.

A malformed manifest is a failed capability of that file; the unit still
seals with the file node, and the provider run continues. Only a snapshot
read failure or a sink failure fails the unit.

## Shared emitter

`filesystem.Emitter` is the one fact builder both providers use. It resolves
candidates, attaches evidence for this unit and run, deduplicates node and
relation facts within the unit (storage keys them per unit), merges the
evidence of an edge asserted more than once, records aliases for every node
it publishes, and hands records to the sink in the order storage requires:
nodes, then relations, aliases and search documents. Streamed chunks are put
after the nodes have been flushed. It also collects the per-file capability
states and the run counters for the `ProviderResult`.
