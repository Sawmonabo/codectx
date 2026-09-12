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

A unit is one file for both providers (ruling R7-1). The coordinator plans:

| Provider | One unit per | Scope key | Inputs | Dependencies |
|---|---|---|---|---|
| `filesystem` | every non-deleted snapshot file | `filesystem.ScopeKey(path)` = `"file:"+path` | that file | none |
| `manifest` | every file with `manifest.Recognize(path)` true | the same scope key | that file | the `filesystem` unit of the same file |

The unit's `InputHash` is the digest of that one file version, so an
unchanged file reuses its sealed unit with zero work, and a change to one
file invalidates exactly two units. `filesystem.PathFromScope` recovers the
path inside `IndexUnit`; a scope that is not a normalized file scope is a
coordinator error.

Both providers are `Required` and always detect as available. `manifest`
detection additionally reports the well-known root manifests it finds
(`go.mod`, `go.work`, `package.json`, `Cargo.toml`, `pyproject.toml`,
`pom.xml`) through `Root.Lstat`; nothing is walked.

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
pure function of the path (ruling R7-2), so every unit that mentions a path
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

`filesystem.Classify` is the single path classification table (ruling
R7-4). `filesystem.Language(path)` is the language tag Tasks 8 and 9 select
files by; the tags for the bundled grammars match `tree_sitter.languages`.
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
line longer than a chunk is split at a UTF-8 boundary. Consecutive chunks
overlap by at most two whole lines and at most 1 KiB, so a pathological line
cannot make the overlap a second copy of the chunk; a chunk that ends inside
a split line has no overlap. The window held in memory is one chunk.

A chunk's search key is `H("search-chunk-v1", file, content hash, start,
end)`; its body is exactly the bytes of `[start,end)`.

Admission affects declared coverage only, never CAS retention or source
serving. The `search` capability is reported per file:

| Condition | State | Diagnostic |
|---|---|---|
| size over `workspace.max_search_file_bytes` | `unavailable` | `CTX_RESOURCE_LIMIT` |
| NUL byte in the first 8000 bytes | `unavailable` | `CTX_PROVIDER_UNAVAILABLE` |
| some chunks were not UTF-8 text (skipped; source reads serve them as base64) | `partial` | `CTX_ARGUMENT_INVALID` |
| otherwise | `fresh` | |

The `structure` capability is always `fresh` for a unit that seals.

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
| `Cargo.toml` | BurntSushi/toml; ranges from a bounded line scan | `package` (or `configuration` for a virtual workspace manifest); a `[package]` field written `{ workspace = true }` is listed in `inherited` and not resolved | `[dependencies]`/`[dev-dependencies]`/`[build-dependencies]` and their `[target.*]` forms → `runtime`/`dev`/`build`; `optional = true` → `optional`; `{ workspace = true }` → `inherited: workspace`, requirement unresolved; `[workspace.dependencies]` → `depends_on` with `declared: workspace` |
| `pyproject.toml` | BurntSushi/toml; per-requirement ranges are the quoted string inside the located array | `package`; `dynamic` fields are metadata and never invented | PEP 621 `dependencies` → `runtime`; `optional-dependencies` → `optional` with `extra`; PEP 735 `dependency-groups` → `dev` with `group`; `build-system.requires` → `build`; Poetry tables → `runtime`/`dev` |
| `pom.xml` | streaming `encoding/xml` tokens with depth (32), element (200k) and text (4 KiB) bounds | `package` `maven:group:artifact`; a missing `groupId`/`version` is taken from `<parent>` and listed in `inherited`; `<properties>` are metadata | `<dependency>` scope `compile`/`runtime` → `runtime`, `test` → `test`, `provided`/`system`/`import` → `build`, `<optional>true` → `optional`; `<parent>` → `depends_on` kind `build`, `role: parent`; `<dependencyManagement>` → `configures` with `managed`; `<modules>` → `builds` the module directory; `${property}` references stay literal with `unresolved: property` |
| Markdown (and ADRs) | bounded line scanner (R7-3) | the `document` node (idempotent with the filesystem fact) | ATX headings → name-only search documents of the document; links and reference definitions whose destination is a workspace path → `documents` to the file or directory, precision `heuristic`; fenced code is skipped; URLs and fragments are not paths |

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
| a list was cut at `MaxDependencies` (4096) or `MaxEntries` (1024) | `partial` | `CTX_RESOURCE_LIMIT` |
| one entry was unusable (a dependency without a name, a path outside the workspace) | `partial` | `CTX_ARGUMENT_INVALID` |
| the file does not parse as its format | `failed` | `CTX_ARGUMENT_INVALID` |
| size over `workspace.max_parse_file_bytes` | `unavailable` | `CTX_RESOURCE_LIMIT` |

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
