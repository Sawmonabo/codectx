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
| Callee reference | function (bare call) or method (qualified call) | `file:<path>` | `call:<name>` or `call:<qualifier>.<name>` | no | `unresolved`; metadata `{"resolution":…,"candidates":…,"callee":…}` |

A callee reference node is minted for every call this file cannot resolve to
exactly one of its own declarations. Its metadata carries the Section 9.3
attribute pair and no number that could be read as a confidence:

| `resolution` | `candidates` | When |
|---|---|---|
| `ambiguous` | number of candidates (≥ 2) | the callee name is declared more than once in this file (overloads, two methods of the same name); the candidates are also retained as `may_refer_to` edges |
| `import` | 0 | the qualifier is a name an import of this file introduced (`fmt.Println`, `str.ToUpper`), so the callee comes through that import and is known not to be here |
| `unresolved` | 0 | nothing in this file declares the name — a builtin, a conversion, a cross-file callee |

`exact` never appears: it requires a compiler binding this provider does not
have. `same_module` and `unique_name` never appear either: a call that does
resolve to one declaration of this file names that declaration directly, and
the `calls` edge to a located declaration in the same file states the
resolution structurally rather than as an attribute on a node that also
describes the declaration itself. This provider never resolves a name outside
the file, so `unique_name` has no meaning here.

Aliases published (so later units and other providers resolve the same
symbols without rescanning):

- `("file:"+path, "module:"+path)` → file module.
- `("file:"+path, qualified name)` → every declaration.
- `("file:"+path, "decl:"+name+"@"+path+":"+startLine+"-"+endLine)` → every
  declaration. This is the one cross-provider declaration key, shared
  byte-for-byte with the `dependence` provider, whose exported
  declaration key is exactly this string:

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
  qualified-name alias either way. The omission is counted, so the file's
  capability state becomes `partial` with `CTX_COVERAGE_INCOMPLETE`: a
  declaration without this key will not merge with the semantic provider's
  node for the same function, which is missing coverage, not a silent
  simplification.
- `("file:"+path, "callsite:"+path+":"+first+"-"+last)` → the callee node of
  every call site. This is the Section 11.3 call-site join key, computed
  byte-for-byte identically by the SCIP importer for the reference occurrence
  at the same bytes, so the reconciler merges the syntactic callee with the
  compiler-resolved symbol and the `calls` relation acquires a precise target
  while both evidence rows remain:

  ```
  scope key  file:<path>
  native key callsite:<path>:<first byte>-<last byte>
  ```

  The range is the **callee identifier token's**, one-based and inclusive: a
  token occupying the half-open UTF-8 byte range `[start,end)` is spelled
  `start+1` `-` `end`. Byte offsets, never characters and never UTF-16 code
  units — the SCIP importer converts its own encoding through
  `internal/source` before it builds the same key. The path is the same
  root-relative slash path the unit's scope key carries. The identifier range
  is validated against the pinned bytes like every other offset, so an offset
  inside a UTF-8 sequence or outside the call is `CTX_PROVIDER_OUTPUT_INVALID`,
  not a key. A key over `MaxNativeKeyBytes` is omitted rather than truncated
  and the omission is counted, so the file is `partial`. Only `@call.name`
  captures produce this alias; a type reference does not, because only a call
  site is the join SCIP cannot make on its own (no indexer distinguishes a
  call-site reference from a function-value reference).
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
| `calls` | enclosing declaration (or file module) → callee reference | a call this file cannot resolve to exactly one of its declarations: ambiguous, import-qualified (`fmt.Println`, `str.ToUpper`, `path.join`, `std::move`) or not declared here. The callee is a provider-local node carrying `resolution`/`candidates`, never a guess |
| `may_refer_to` | enclosing declaration → each candidate | a call whose name is declared more than once in this file (overloads, shadowing); bounded by `MaxAmbiguousCandidates`, alongside the `calls` edge to the ambiguous callee reference |
| `references` | enclosing declaration → type declaration | a type reference to a class/struct/interface/enum declared in this file; a type this file does not declare gets no placeholder |
| `may_refer_to` | declaration → alias alternative | the resolver returned an ambiguous alias match |

Occurrences past `MaxEvidencePerFact` (65536, or the user-set `index.max_evidence_per_fact`) on one relation or node are
counted and the file's capability state becomes `partial`; nothing is dropped
silently. That count is disclosed on its own, under the `evidence_clipped`
capability detail -- the same key the filesystem provider reports the same
bound with -- so an operator can tell how many occurrences their clip cut apart
from every other bound this file reached. The unresolved callees one file may mint are unbounded by default; an
operator who sets `providers.tree_sitter.max_callee_references` bounds the
distinct placeholders one file mints, and past it a cross-file call is counted,
not minted, and the file is `partial`.

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
| `partial` | `CTX_COVERAGE_INCOMPLETE` | tree contains ERROR/MISSING nodes, the query exceeded its match limit, evidence past the per-fact bound was cut (counted under the `evidence_clipped` detail), the file reached a user-set `providers.tree_sitter.max_callee_references` bound, or a declaration's cross-provider key was over `MaxNativeKeyBytes` and omitted |
| `unavailable` | `CTX_RESOURCE_LIMIT` | file larger than a user-set `workspace.max_parse_file_bytes`, or longer than the parser's 32-bit offsets address (`wire.MaxSourceOffset`); not streamed |
| `unavailable` | `CTX_PROVIDER_UNAVAILABLE` | not valid UTF-8, or no pinned grammar for the file |

Failures that fail the unit (output deleted, never sealed): a fact the worker
sent that does not describe the pinned bytes (`CTX_PROVIDER_OUTPUT_INVALID`:
range outside the file, offset inside a UTF-8 sequence, parent that does not
contain its child, unknown kind, over-long name), a worker that failed twice
(`CTX_PROVIDER_UNAVAILABLE`, retryable), a worker the hang detector ended
(`CTX_PROVIDER_TIMEOUT`, retryable, detail `stop_reason` = `stalled`), cancellation (`CTX_CANCELED`), a blob whose length
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
Stdin/Stdout            io.Pipe, no byte total on either
MaxStderrBytes          16 KiB
Grace                   2 seconds (no timeout)
CPUProgress             the worker's processor time, read by the hang detector
MemoryReservationBytes  Options.WorkerMemoryBytes (the composition passes 256 MiB)
```

Before it is started, each worker reserves `Options.WorkerMemoryBytes` on the
process's one admission ledger (`Options.Admission`), so parser workers wait in
the same queue, against the same allocation, as every other heavy child; the
reservation is given back once the runner has reaped the worker. Every
admission is first-in-first-out on the ledger, and a worker coming back from a
parse while an acquirer of the pool is queued there follows that order:

- When the pool's acquirer is the ledger's head (the ledger tells it so
  through the make-room step, once it is the head and does not fit), the
  worker is handed to it together with the reservation it holds. That is the
  grant the ledger would make, without stopping a process to start the same
  one again, so an allocation that fits fewer workers than acquirers (an
  observed zero included) keeps its workers warm and runs them one handout at
  a time.
- When another reserver is the head, the worker is stopped: its reservation
  returns to the ledger, the ledger pumps, and that head is admitted first.
  The pool's acquirer behind it waits its turn.

No idle worker is held or reused while any reserver waits on the ledger, the
pool's own or another. The ledger reports whether one is waiting
(`admission.Ledger.Waiting`), and the pool is registered on it as a holder of
idle room (`admission.Ledger.Holder`) for its whole life:

- A worker coming back while a reserver waits, and no acquirer of the pool is
  the head, is stopped rather than idled.
- An acquirer that finds idle workers while a reserver waits stops every one
  of them and then queues for a worker of its own behind that reserver.
- Whenever the ledger's head does not fit, the head's waiting goroutine runs
  the pool's idle-release step, with no ledger lock held, and the pool stops
  its idle workers at once, whether or not it is acquiring or releasing
  anything. A stage whose own progress waits on that reserver (a unit of the
  same tick reserving behind the room idle workers hold) never waits on the
  stage's drain.

The stopped workers' room returns to the ledger and reaches the head in order.
With nobody waiting, a worker coming back goes idle and is held for the stage
like any room an admitted child holds. Room is returned by its holder, never
taken from it, only idle room is given back, and the order among the reservers
queued on the ledger is kept throughout.

The runner holds one concurrency slot per live worker for the worker's whole
life. The composition gives the workers a runner of their own, with
`process.Limits.MaxConcurrent` equal to the worker count and a memory budget
that holds every reservation the admission ledger can have granted the workers
at once, so the runner never refuses a worker the ledger has admitted.

Wiring in `cmd/codectx/main.go`:

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
	MaxWorkers:        config.ParserWorkers(),
	MaxParseFileBytes: cfg.Workspace.MaxParseFileBytes,
	WorkerMemoryBytes: parserWorkerReservationBytes,
	Admission:         admissionLedger,
	Worker:            treesitter.WorkerCommand{Path: exe, Args: []string{wire.Subcommand}},
	Runner:            parserRunner,
	WorkDir:           filepath.Join(dataDir, "workers", "treesitter"),
})
defer ts.Close()
```

### Protocol (`wire`)

Every frame is a 4-byte big-endian payload length, a 1-byte kind (whose high
bit says the message continues in the next frame) and a slice of the message.
`wire.ChunkBytes` = 64 KiB is the transport unit, not a limit on anything
carried: a message of any length is sent as as many frames as it needs, and a
reader never allocates for a frame longer than one unit, so a corrupt header
costs it at most one unit before it is refused as malformed. There is no frame
cap and no source cap: the source is streamed in chunks, and no record is
dropped or refused for its size on the wire. `workspace.max_parse_file_bytes`
is the only size policy, enforced by the parent before any request is sent;
the one other refusal is a file longer than the parser's 32-bit offsets
address (`wire.MaxSourceOffset`), which is reported unavailable rather than
published with wrapped offsets.

The transport does not bound the sink record a message becomes; the parent's
own bounds do. The largest fact the parent can build is one node or relation
carrying `MaxEvidencePerFact` (65536, or the user-set `index.max_evidence_per_fact`) evidence rows of roughly 620 bytes of
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
                  Ref{kind, start, end, name_start, name_end, name, scope, qualified?, qualifier?, qualifier_is_import?}
worker → parent   Done{package, syntax_errors, truncated, rss_bytes}
              or  Error{code, message}                         per-file failure; worker stays healthy
```

A `Ref` carries two ranges: `start`/`end` bound the whole reference
expression (the call expression for a call), and `name_start`/`name_end`
bound the callee identifier token alone. The alias is built from the second,
because that is what a SCIP occurrence covers; the enclosing expression's
range would join nothing. `qualifier` is the receiver as written when it is a
name within `wire.MaxQualifierBytes` and empty when the receiver is a larger
expression — a chained `a.b(x).c(y).Scan(&v)` has a receiver hundreds of bytes
long, which is a callee this file cannot name, not a string to truncate.
`qualifier_is_import` is decided from the receiver's full text either way.

Fact frames carry byte offsets and names only. The parent recomputes every
line and column from the pinned bytes with `source.Cursor` (the single
position implementation), rejects offsets inside a UTF-8 sequence, checks
range containment and every string bound, and maps declaration kinds through
a closed vocabulary. There is no per-file record bound: what the parent holds
for one file's answer is the records the worker extracted from that file, a
function of the file's bytes.

### Lifecycle

- `MaxWorkers` bounds live worker *processes*, not concurrent parses: a worker
  occupies its place in the pool from before it is started until the runner has
  reaped it, so an idle worker and one still shutting down both still count. A
  unit reuses an idle worker, starts one when the pool is under its bound, or
  waits (promptly returning on cancellation) until a worker goes idle or a
  process exits; it then reads the hello under the hang detector below. A worker stays warm
  only while there is parse work in flight: the last unit to finish drains the
  pool, so a process that has stopped parsing holds none. There is no idle
  timer, because a timer would only choose how long a resting machine carries
  one worker per core to save the milliseconds a restart costs. Bounding callers
  instead would let a caller start a fresh worker while an expiring one still
  held its runner slot and memory reservation, and the runner would then refuse
  an admission the pool itself caused.
- A healthy worker returns to the idle list, where the next parse of the
  stage reuses it; the drain that follows the last caller closes its stdin,
  the worker exits on EOF and the runner reaps it. An unhealthy worker is
  stopped at release instead. While any reserver waits on the admission
  ledger, a healthy worker is handed to the pool's acquirer when that acquirer
  is the ledger's head and stopped otherwise, and idle workers are stopped
  rather than reused (see Launch). A worker has no lifetime, no parse count, no
  per-parse deadline and no lifetime byte total: a long parse of a large file
  is not a hung one, and only progress can tell the two apart.
- The hang detector runs while a request or the hello is outstanding. A
  worker is alive for as long as its processor time moves or a byte of its
  answer arrives, and is ended only when both have stood still for one window
  (one minute). Where the platform publishes no processor-time reading --
  before the runner has seen the process, or for the whole run where it cannot
  sample a running tree -- the detector never fires, because silence alone
  cannot tell a long parse from a wedged one. The failure is
  `CTX_PROVIDER_TIMEOUT` with `stop_reason` = `stalled`. There is no
  configuration key: it is a window, not a limit.
- Cancellation or the hang detector kills the worker through the runner
  (process tree termination) and closes the parent's ends of its pipes at once,
  so the runner's stdin copy sees EOF instead of waiting out its grace. The
  worker has no in-process cancellation path and needs none.
- A worker that breaks protocol or dies mid-parse is stopped and, after a
  successful hello, the parse is retried exactly once on a fresh worker.
  Startup failures (the runner's trust refusal, admission refusal, a missing
  hello, a fingerprint mismatch), per-file error frames, cancellation and a
  stalled worker are not retried.
- `Close` closes every idle worker's stdin — concurrently, since each exits on
  EOF within the grace and serial stops would cost one grace after another —
  kills every worker that is not idle, and waits for the runner to reap each.
- `Stats()` is the aggregate accounting of Section 22: `Processes` (every
  worker process the runner has not yet reaped, idle ones included, never more
  than the worker count), `IdleWorkers` (its reusable subset) and
  `BusyWorkers` (the rest: parsing, or on their way out), started and exited
  counts, parses, retries, the sum of the resident set each live worker last
  reported, the parent's own resident set and the live worker PIDs. Both sides
  measure RSS with the same `wire.ResidentBytes` helper over
  `/proc/self/statm`. A value that cannot be measured is -1, never 0.

### Native lifecycle in the worker

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
| `@bind.names` + `@bind.values` | the name list and the value list of a Go short variable declaration whose values hold a function literal, each captured once per statement under `@def.function`; the worker pairs the i-th name with the i-th value and declares each identifier bound to a function literal, so `x, f := 1, func() {}` declares f alone. The value list must hold a function literal for the pattern to match, so a plain `x := v` is never walked |
| `@import.clause` | an ECMAScript import clause, captured once per statement; the worker walks its default, namespace and named bindings in source order. No pattern captures each entry of a list and then a later sibling of that list: the matcher would keep one partial match per entry open until the sibling, which is quadratic in the entry count |
| `@call` + `@call.name` + `@call.qualifier` | a call site; `@call.name` is the callee identifier whose byte range becomes the `callsite:` alias, so every language pack must capture it (TSX and C++ inherit theirs from `ecmascript.scm` and `c.scm`) |
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
| file size | `workspace.max_parse_file_bytes` (unlimited by default); a file past the parser's 32-bit offsets (`wire.MaxSourceOffset`) is unavailable | parent, before any request |
| transport unit | `wire.ChunkBytes` (64 KiB) per frame; a message continues across frames | both sides |
| evidence per fact | `index.max_evidence_per_fact`, or the model's record ceiling (65536) when unset | parent |
| distinct callee reference nodes per file | `providers.tree_sitter.max_callee_references` (unlimited by default) | parent; past a set bound the call is counted, not minted, and the file is `partial` |
| call qualifier (receiver text) | `wire.MaxQualifierBytes` (512) | worker; a larger receiver expression is reported as an unnamed qualifier, so the key is `call:.<name>` |
| call-site alias key | `MaxNativeKeyBytes` (2048) | parent; past it the key is omitted, not truncated, and the file is `partial` |
| cross-provider declaration key | `MaxNativeKeyBytes` (2048) | parent; past it the key is omitted, not truncated, and the file is `partial` |
| records per sink hand-off | 1000 | parent |
| documentation body | 8 KiB | parent |
| signature | `MaxSignatureBytes` (4 KiB), whitespace-collapsed | parent |
| workers | one per CPU (`config.ParserWorkers()`) | pool |
| hang detector | one minute with neither processor time nor answer bytes moving, while a request or the hello is outstanding; never fires without a processor-time reading | pool; kills the worker, `CTX_PROVIDER_TIMEOUT` `stop_reason` = `stalled` |
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

`THIRD_PARTY_LICENSES.md` at the repository root carries this inventory.

## Tests

- `internal/provider/treesitter/treesitter_test.go` `TestLanguageFixtures`:
  one fixture per grammar (nested declaration, non-ASCII range) through
  `providertest.Conform`, then a captured run asserting every located node's
  byte range selects source containing its name, lines match the bytes, the
  nested declaration is contained by its outer one, the pool never exceeds
  its bound and every worker has exited after `Close`. The test binary is its
  own worker through `TestMain`. `checkCallsites` extends the same loop with
  the three call-site join invariants: every `callsite:` key's one-based
  inclusive range selects exactly its callee node's name in the pinned bytes
  (the Go fixture's non-ASCII `日本語` callee is what separates a byte range
  from a rune range at both ends), a second independent unit over the same
  bytes publishes identical keys naming identical identities, and the call to
  the builtin `len` publishes both its alias and a callee node with
  `resolution=unresolved`, `candidates=0`.
- `internal/bench` `TestParserResourcePlateau` (skipped under `-short`): 600
  parses of 64×-repeated fixtures through the real worker path on one
  long-lived worker; worker RSS after warm-up must stay within 8 MiB of its
  first post-warm-up sample; every worker PID must be gone after `Close`.
