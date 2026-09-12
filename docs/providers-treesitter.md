# Provider: `treesitter`

The bundled structural provider (implementation plan Section 11.3). It parses
each supported source file with a pinned tree-sitter grammar in an isolated
worker subprocess and publishes declarations, containing scopes, signatures,
imports and exports, syntax references and call sites, test declarations and
attached documentation at precision `syntax`. It is required, file-scoped and
depends on the `filesystem` provider by ID.

Packages:

| Package | Role |
|---|---|
| `internal/provider/treesitter` | Parent side: worker pool, fact validation, resolution, emission. Never links a grammar. |
| `internal/provider/treesitter/worker` | Child side: `Main(ctx, stdin, stdout, stderr) int`. Owns every native object. |
| `internal/provider/treesitter/wire` | Length-prefixed JSON framing shared by both. |
| `internal/provider/treesitter/lang` | Pin table (grammar module, ABI, version), embedded query packs, fingerprint. |
| `internal/bench` | `TestParserResourcePlateau` (skipped under `-short`). |

## Descriptor

```
ID                treesitter
Version           1.<extraction-version>-<fingerprint[:24]>
Capabilities      structure
DependsOn         filesystem
InvalidationScope file
Required          true
```

`lang.Fingerprint()` folds, with `model.Hasher` under the domain
`treesitter-fingerprint-v1`: the binding module and version
(`github.com/tree-sitter/go-tree-sitter@v0.25.0`), the extraction version, and
for every language its name, grammar module and version, ABI, grammar metadata
version and the full text of its composed query pack. A grammar upgrade, an ABI
change or an edited `.scm` therefore changes `ProviderVersion`, which changes
every unit key (`NewUnitID` folds the version) and invalidates all sealed
treesitter units, as Section 11.3 requires. The worker reports the same
fingerprint in its hello frame and the parent refuses a worker whose value
differs, so a stale binary can never answer for a newer parent.

### Pinned grammars

| Language | Module | ABI | Metadata | Query packs |
|---|---|---|---|---|
| c | tree-sitter-c v0.24.2 | 15 | 0.24.2 | c |
| cpp | tree-sitter-cpp v0.23.4 | 14 | – | c + cpp |
| go | tree-sitter-go v0.25.0 | 15 | 0.25.0 | go |
| java | tree-sitter-java v0.23.5 | 14 | – | java |
| javascript | tree-sitter-javascript v0.25.0 | 15 | 0.25.0 | ecmascript + javascript |
| python | tree-sitter-python v0.25.0 | 15 | 0.25.0 | python |
| rust | tree-sitter-rust v0.24.2 (reports 0.24.1) | 15 | 0.24.1 | rust |
| typescript | tree-sitter-typescript v0.23.2 (typescript) | 14 | – | ecmascript + typescript |
| tsx | tree-sitter-typescript v0.23.2 (tsx) | 14 | – | ecmascript + typescript |

The worker verifies every linked grammar's ABI and, where the grammar carries
`ts_language_metadata`, its version against this table before it sends hello;
a mismatch is exit code 2 and the parent reports `CTX_PROVIDER_UNAVAILABLE`
with the worker's first stderr line. The exact modules and versions are in
`internal/provider/treesitter/lang/lang.go`; `go.mod` is the source of truth
for what is linked.

### Detection

`Detect` inspects nothing in the repository: the grammars are linked into the
worker executable, so availability is decided by that file alone (a regular,
executable path). It never opens a repository file and never walks the root. A
repository with no supported source file therefore reports the provider
`available` and simply gets zero units planned for it, which is the honest
outcome — an available provider with nothing to do, not an unavailable one.

## Unit scope and node identity

One unit per file, scope key `"file:" + path`. The provider reads the scope
key, looks the path up in the pinned snapshot (`EachFile` with a one-path
selection, never a manifest walk), reads the bytes through `SnapshotView.Open`
bounded by the manifest size, and hands them to a worker. Nothing is read from
the live checkout and nothing survives the unit: there is no repository-wide
AST or source cache.

Every identity comes from `req.Resolver.Resolve`; the provider copies
`Resolution.CanonicalKey` verbatim. The candidate shapes, and therefore the
minted key basis, per node kind:

| Node | Kind | ScopeKey | NativeKey | Located | Basis when minted |
|---|---|---|---|---|---|
| File module | `module` | `file:<path>` | `module:<path>` | yes, whole file | `source_location` |
| Declaration | function, method, class, interface, struct, enum, field, variable, constant, test, module, namespace | `file:<path>` | qualified name | yes, exact declaration range | `source_location` |
| Import target | `module` | `provider.ScopeWorkspace` | `import:<lang>:<import path>` | no; qualified name = import path | `structural_key` (same import path from any file mints the same node) |
| Unresolved callee | function (bare call) or method (qualified call) | `file:<path>` | `call:<name>` or `call:<qualifier>.<name>` | no | `unresolved`; metadata `{"resolution":"unresolved"}` |

Aliases published (so later units and other providers resolve the same
symbols without rescanning):

- `("file:"+path, "module:"+path)` → file module.
- `("file:"+path, qualified name)` → every declaration.
- `("file:"+path, "decl:"+name+"@"+path+":"+startLine+"-"+endLine)` → every
  declaration. This is the cross-provider declaration key fixed by controller
  ruling, shared byte-for-byte with the `joern` provider, whose exported
  `METHOD` strong key is exactly this string:

  ```
  scope key  file:<path>
  native key decl:<identifier token as written>@<path>:<first line>-<last line>
  ```

  Lines are one-based and the span is inclusive, so the last line is the line
  the declaration's final byte lies on, not the half-open end position's. The
  path is the same root-relative slash path the unit's scope key carries.
  Nothing is normalized — no case folding, no qualification, no receiver
  prefix: the identifier is the token the source spells. Publishing this alias
  is what merges the two providers' identities for one function instead of
  leaving two correct but unrelated nodes. A key over `MaxNativeKeyBytes` is
  omitted rather than truncated (a truncated key would be a different, possibly
  colliding claim); the declaration keeps its own identity and its
  qualified-name alias either way.
- `("pkg:go:"+dir+":"+package, qualified name)` → Go top-level declarations;
  `("pkg:java:"+package, qualified name)` → Java top-level declarations. These
  are the two languages where a declared package clause makes members visible
  across files without an import. Other languages get no package-scope alias
  from this provider: guessing a module key for Python or JavaScript from a
  path would be the cross-file inference the plan forbids at precision syntax.

Relations, all with `syntax` evidence carrying the exact byte range:

| Relation | From → To | When |
|---|---|---|
| `defines` | file module → declaration | top-level declaration |
| `contains` | outer declaration → nested declaration | nested declaration (parent is the innermost containing declaration; a Rust `impl` block or C++ qualifier contributes to the qualified name and makes the function a method but is not itself a node) |
| `exports` | file module → declaration | Go exported identifier, Rust `pub`, Java `public`, JS/TS `export` (including `export { x }` lists), TS `export default` |
| `imports` | file module → import target | every import/include/use |
| `calls` | enclosing declaration (or file module) → declaration | a call whose name is declared exactly once in this file, respecting qualification (a `recv.name()` call names methods; a bare call names functions, tests, classes and structs); definitions preferred over prototypes |
| `may_refer_to` | enclosing declaration → each candidate | a call whose name is declared more than once in this file (overloads, shadowing); bounded by `MaxAmbiguousCandidates` |
| `calls` | enclosing declaration → unresolved callee | a call whose name is not declared in this file, or whose qualifier is a name an import introduced (`fmt.Println`, `str.ToUpper`, `path.join`, `std::move`): cross-file, so never guessed |
| `references` | enclosing declaration → type declaration | a type reference to a class/struct/interface/enum declared in this file; a type this file does not declare gets no placeholder |
| `may_refer_to` | declaration → alias alternative | the resolver returned an ambiguous alias match |

Occurrences past `MaxEvidencePerFact` (64) on one relation or node are
counted and the file's capability state becomes `partial`; nothing is dropped
silently. The unresolved callees one file may mint are bounded the same way: at
most 2000 distinct placeholders, after which a cross-file call is counted, not
minted, and the file is `partial`. Without that bound a generated or minified
file with tens of thousands of distinct callee names would publish a node, a
relation and evidence for each.

Search units: one per declaration, ID `H("treesitter-search-v1", fileID,
nodeID)`, body = attached documentation (comment markers stripped) plus the
collapsed signature, bounded to 8 KiB; bodies of declarations are not
duplicated (Section 11.2 stores source once, in the filesystem unit).

### Capability state per file

The run always reports `succeeded` (so the unit seals and coverage is
recorded) with one `structure` capability state at the file's scope:

| State | DiagnosticCode | Meaning |
|---|---|---|
| `fresh` | – | parsed without syntax errors, every record within bounds |
| `partial` | `CTX_COVERAGE_INCOMPLETE` | tree contains ERROR/MISSING nodes, a per-file record bound was reached, a record was over the frame cap, evidence past the per-fact bound was dropped, or the file reached the 2000 distinct unresolved-callee bound |
| `unavailable` | `CTX_RESOURCE_LIMIT` | file larger than `workspace.max_parse_file_bytes`; not streamed |
| `unavailable` | `CTX_PROVIDER_UNAVAILABLE` | not valid UTF-8, or no pinned grammar for the file |

Failures that fail the unit (output deleted, never sealed): a fact the worker
sent that does not describe the pinned bytes (`CTX_PROVIDER_OUTPUT_INVALID`:
range outside the file, offset inside a UTF-8 sequence, parent that does not
contain its child, unknown kind, over-long name), a worker that failed twice
(`CTX_PROVIDER_UNAVAILABLE`, retryable), a parse over `ParseTimeout`
(`CTX_PROVIDER_TIMEOUT`), cancellation (`CTX_CANCELED`), a blob whose length
disagrees with the manifest (`CTX_SOURCE_INTEGRITY`).

### Language detection

`FileVersion.Language` from the snapshot manifest wins when it names a pinned
language; otherwise the extension decides (`.go .py .pyi .js .mjs .cjs .jsx
.ts .mts .cts .tsx .java .rs .c .h .cc .cpp .cxx .hpp .hh .hxx`). `.h` is parsed as
C: a C parse of a C++ header yields ERROR nodes and a `partial` state with
`CTX_COVERAGE_INCOMPLETE`, which is an honest report and the expected outcome
for a C++ header named `.h`; guessing C++ from a neighbouring `.cpp` would not
be.

## Worker process

### Launch

Workers are started only through the shared `internal/process` runner with:

```
Path                    WorkerCommand.Path   (production: this binary; tests: the test binary)
Args                    WorkerCommand.Args   (production: ["__ts-worker"])
Dir                     Options.WorkDir      (absolute private directory)
Env                     none
Stdin/MaxStdinBytes     io.Pipe, 1 GiB lifetime budget
Stdout/MaxStdoutBytes   io.Pipe, 1 GiB lifetime budget
MaxStderrBytes          16 KiB
Timeout/Grace           1 hour / 2 seconds
MemoryReservationBytes  Options.WorkerMemoryBytes (default 256 MiB)
```

The runner holds one concurrency slot per live worker for the worker's whole
lifetime, so `process.Limits.MaxConcurrent` must exceed
`index.max_parser_workers` by the number of other children expected to run
concurrently.

Wiring for `cmd/codectx/main.go` (the controller applies it; this package does
not edit `cmd/`):

```go
if len(os.Args) > 1 && os.Args[1] == wire.Subcommand {
	os.Exit(worker.Main(context.Background(), os.Stdin, os.Stdout, os.Stderr))
}
```

and in application composition:

```go
exe, _ := os.Executable()          // then filepath.EvalSymlinks
ts, err := treesitter.New(treesitter.Options{
	Languages:         cfg.Providers.TreeSitter.Languages,
	MaxWorkers:        cfg.Index.MaxParserWorkers,
	MaxParseFileBytes: cfg.Workspace.MaxParseFileBytes,
	WorkerIdleTTL:     time.Duration(cfg.Providers.TreeSitter.WorkerIdleTTL),
	Worker:            treesitter.WorkerCommand{Path: exe, Args: []string{wire.Subcommand}},
	Runner:            sharedRunner,
	WorkDir:           filepath.Join(dataDir, "workers", "treesitter"),
})
defer ts.Close()
```

### Protocol (`wire`)

Every frame is a 4-byte big-endian payload length, a 1-byte kind and a JSON
payload (ruling R8-3). Both sides refuse to allocate for a length over the
cap they expect: the parent caps every frame from the child at
`wire.MaxFactFrameBytes` = 64 KiB; the worker caps a request at the same and a source
frame at the `SourceBytes` the request declared, itself at most
`wire.MaxSourceBytes` = 64 MiB. The parent enforces
`workspace.max_parse_file_bytes` before any request is sent.

The frame cap does not bound the sink record a frame becomes; the parent's own
bounds do. The largest fact the parent can build is one node or relation
carrying `MaxEvidencePerFact` (64) evidence rows of roughly 620 bytes of
identifiers and positions plus a native key of at most `MaxNativeKeyBytes`
(2048): about 167 KiB, well under the 4 MiB `max_provider_record_bytes` a sink
is configured with. Facts are handed to the sink in slices of at most 1000
records (the default `index.batch_records`, which the `Sink` interface does not
expose), in put order, and the builder drops its reference to each group as it
hands it over, so a unit's output is never held twice.

```
worker → parent   Hello{pid, fingerprint, languages}          once, first
parent → worker   Request{language, path, source_bytes}
parent → worker   Source<raw bytes>                            exactly source_bytes
worker → parent   Decl* Import* Ref*                           facts, one record each
worker → parent   Done{package, syntax_errors, truncated, rss_bytes}
              or  Error{code, message}                         per-file failure; worker stays healthy
```

Fact frames carry byte offsets and names only. The parent recomputes every
line and column from the pinned bytes with `source.Cursor` (the single
position implementation), rejects offsets inside a UTF-8 sequence, checks
range containment and every string bound, and maps declaration kinds through
a closed vocabulary. Per-file record bounds (`MaxDeclsPerFile` 20000,
`MaxImportsPerFile` 4000, `MaxRefsPerFile` 60000) are applied by the worker
when extracting and by the parent when reading, so a misbehaving child cannot
make the parent buffer more than a healthy one would send. A record whose
encoding would exceed the frame cap is dropped by the worker and reported as
truncation, never sent oversize.

### Lifecycle

- `MaxWorkers` bounds live worker *processes*, not concurrent parses: a worker
  occupies its place in the pool from before it is started until the runner has
  reaped it, so an idle worker and one still shutting down both still count. A
  unit reuses an idle worker, starts one when the pool is under its bound, or
  waits (promptly returning on cancellation) until a worker goes idle or a
  process exits; it then reads the hello within 30 seconds. Bounding callers
  instead would let a caller start a fresh worker while an expiring one still
  held its runner slot and memory reservation, and the runner would then refuse
  an admission the pool itself caused.
- A healthy worker returns to the idle list under `WorkerIdleTTL`; expiry
  closes its stdin, the worker exits on EOF and the runner reaps it.
- A worker is recycled (stopped after its current parse) when it has done
  2048 parses, consumed three quarters of its stdin or stdout lifetime budget,
  or lived three quarters of its hour, so a healthy worker is never killed by
  its own budget mid-parse. The parse count bound is defence in depth against
  any per-parse leak in the native binding: whatever leaks is released with
  the process.
- Cancellation or the parse deadline kills the worker through the runner
  (process tree termination) and closes the parent's end of its stdin at once
  so the runner's stdin copy sees EOF instead of waiting out its grace. The
  worker has no in-process cancellation path and needs none.
- A worker that breaks protocol or dies mid-parse is stopped and, after a
  successful hello, the parse is retried exactly once on a fresh worker.
  Startup failures (the runner's trust refusal, admission refusal, a missing
  hello, a fingerprint mismatch), per-file error frames, cancellation and
  timeouts are not retried.
- `Close` closes every idle worker's stdin — concurrently, since each exits on
  EOF within the grace and serial stops would cost one grace after another —
  kills every worker that is not idle, and waits for the runner to reap each.
- `Stats()` is the aggregate accounting of Section 22: `Processes` (every
  worker process the runner has not yet reaped, idle ones included, never more
  than `index.max_parser_workers`), `IdleWorkers` (its reusable subset) and
  `BusyWorkers` (the rest: parsing, or on their way out), started and exited
  counts, parses, retries, the sum of the resident set each live worker last
  reported, the parent's own resident set and the live worker PIDs. Both sides
  measure RSS with the same `wire.ResidentBytes` helper over
  `/proc/self/statm`. A value that cannot be measured is -1, never 0.

### Native lifecycle in the worker (ruling R8-2)

Files read in full from `github.com/tree-sitter/go-tree-sitter@v0.25.0` before
choosing APIs: `parser.go`, `tree.go`, `query.go`, `node.go`,
`tree_cursor.go`, `language.go`, `allocator.go`, `point.go`, `ranges.go`, and
`github.com/mattn/go-pointer` `pointer.go`.

- `Parser.Close`, `Tree.Close`, `Query.Close`, `QueryCursor.Close` free the C
  objects; nothing is finalized by the garbage collector, so the worker keeps
  one parser and one compiled query per language for its lifetime, creates one
  query cursor per parse and closes it before the tree, and closes the tree
  with `defer` so every path (success, error frame, write failure) releases it.
  Everything else is released when the parent terminates the process.
- Parsing uses `Parser.ParseWithOptions(readCallback, nil, nil)`. The
  `options` path (`parser.go:350`) saves a cgo pointer per call with
  `pointer.Save` and never `Unref`s it, so a non-nil options struct leaks per
  parse; it is not used. The deprecated timeout API, the cancellation flag and
  `ParseCtx` are not used either: cancellation is the parent's, by killing the
  process.
- The read callback returns 64 KiB chunks. `readUTF8` copies whatever the
  callback returns into a C string that lives until the parse ends, so a
  whole-file return would double the file's resident cost.
- `QueryCursor.Matches` is used rather than `Captures`: `QueryCaptures.Next`
  (`query.go:1049`) mallocs a `TSQueryMatch` it never frees.
  `MatchesWithOptions` shares the `pointer.Save` leak and is not used.
- `Parser.Reset` is called when a parse returns no tree, so a later parse does
  not resume a half-finished one.

## Query packs

`internal/provider/treesitter/lang/queries/*.scm`, embedded and composed per
language (`ecmascript.scm` is shared by JavaScript, TypeScript and TSX;
`c.scm` by C and C++). The capture vocabulary the worker interprets:

| Capture | Meaning |
|---|---|
| `@def.<kind>` | a declaration of `<kind>` (function, method, class, interface, struct, enum, field, variable, constant, module, namespace, test); its `@name` (or `@declarator` for C) names it, `@body` marks where the signature ends |
| `@scope` + `@scope.name` | a non-declaration container that contributes to qualified names and turns functions into methods (Rust `impl`) |
| `@import` + `@import.path` + `@import.name` | an import statement, its path and the local name it introduces |
| `@call` + `@call.name` + `@call.qualifier` | a call site |
| `@ref.type` | a type reference |
| `@package` | the package/module clause |
| `@export` + `@export.name` | an export wrapper or export list entry |
| `@def.test` + `@test.name` | a test declaration by call form (`it`, `test`, `describe`) |

Per-language hooks in `worker/grammars.go` refine kinds (Go `type_spec`
bodies decide struct/interface; C declarator chains decide prototype,
function or method), decide exports (Go capitalization, Rust `pub`, Java
`public`, JS/TS `export` wrappers), recognise tests (Go `Test*` in `_test.go`,
Python `test_*` functions and `Test*` classes, Java `@Test`, Rust `#[test]`, JS/TS `it`/`test`/`describe`)
and attach documentation (adjacent preceding comments; Python docstrings).

## Limits

| Bound | Value | Where |
|---|---|---|
| file size | `workspace.max_parse_file_bytes` (default 5 MiB), ≤ 64 MiB | parent, before any request |
| fact frame | 64 KiB | both sides |
| declarations / imports / references per file | 20000 / 4000 / 60000 | both sides |
| evidence per fact | 64 | parent |
| distinct unresolved callees per file | 2000 | parent; past it the file is `partial` |
| records per sink hand-off | 1000 | parent |
| documentation body | 8 KiB | parent |
| signature | `MaxSignatureBytes` (4 KiB), whitespace-collapsed | parent |
| workers | `index.max_parser_workers` (default 2) | pool |
| parse time | `Options.ParseTimeout` (default 60 s) | pool; kills the worker |
| worker lifetime | 1 h / 2048 parses / 1 GiB each way | pool; recycled at three quarters |
| hello | 30 s | pool |
| worker stderr | 16 KiB | runner |

## Third-party licenses

All MIT:

- `github.com/tree-sitter/go-tree-sitter` v0.25.0 (binding), Amaan Qureshi
  and tree-sitter contributors; bundles the tree-sitter runtime (MIT, Max
  Brunsfeld).
- `github.com/tree-sitter/tree-sitter-c` v0.24.2, `tree-sitter-cpp` v0.23.4,
  `tree-sitter-go` v0.25.0, `tree-sitter-java` v0.23.5,
  `tree-sitter-javascript` v0.25.0, `tree-sitter-python` v0.25.0,
  `tree-sitter-rust` v0.24.2, `tree-sitter-typescript` v0.23.2 — Max
  Brunsfeld, Ayman Nadeem, Maxim Sokolov and tree-sitter contributors.
- `github.com/mattn/go-pointer` (binding dependency), Yasuhiro Matsumoto.

`THIRD_PARTY_LICENSES.md` at the repository root is a shared file and should
carry this inventory; it is reported as a shared change in the Task 8 report.

## Tests

- `internal/provider/treesitter/treesitter_test.go` `TestLanguageFixtures`:
  one fixture per grammar (nested declaration, non-ASCII range) through
  `providertest.Conform`, then a captured run asserting every located node's
  byte range selects source containing its name, lines match the bytes, the
  nested declaration is contained by its outer one, the pool never exceeds
  its bound and every worker has exited after `Close`. The test binary is its
  own worker through `TestMain`.
- `internal/bench` `TestParserResourcePlateau` (skipped under `-short`): 600
  parses of 64×-repeated fixtures through the real worker path on one
  long-lived worker; worker RSS after warm-up must stay within 8 MiB of its
  first post-warm-up sample; every worker PID must be gone after `Close`.
