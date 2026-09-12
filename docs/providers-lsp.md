# LSP snapshot-qualified working-tree overlay

`internal/provider/lsp` is the Section 11.5 query overlay: on-demand
definition, references, implementations, type definition, document and
workspace symbols and call hierarchy, answered by an approved local language
server running against a **private materialization of the pinned snapshot**.
It is not a `provider.Provider`. It writes nothing to storage, mints no
identities and emits no facts through a sink; every answer is ephemeral,
labelled with the server, its version and an input digest, and carries
`language_server` precision (`lsp.Precision`) and `semantic_source=lsp`
(`lsp.SemanticSource`). When the server stops the answers are simply gone;
canonical generations never change.

## What runs, and why it is allowed to

A `Definition` is what this build knows about a supported server: name,
manifest language tags, the stdio argument array and root markers.
`Definitions()` lists the six of Section 11.5. A definition **authorizes
nothing**. `Definition.Detect(root)` reports which root markers exist, reading
metadata through the confined `workspace.Root` only; it is a hint for choosing
among trusted profiles and never a reason to execute.

A `Profile` is runnable, and `Trusted(cfg, name)` is its only constructor:

| Condition | Result |
|---|---|
| `providers.lsp.enabled = false` | `CTX_PROVIDER_UNAVAILABLE` |
| `name` is not one of the six definitions | `CTX_ARGUMENT_INVALID` |
| no `[analyzers.<name>]` in the **user** configuration | `CTX_TRUST_REQUIRED` |
| approval lacks absolute executable, version constraint, work dir, timeout or budgets | `CTX_TRUST_REQUIRED` |
| args use `${output_file}` or `${manifest}` | `CTX_ARGUMENT_INVALID` |

`enabled = "auto"` therefore means "use an already approved profile"; nothing
found on `PATH` is ever started (Section 20.2). The approval's `args` replace
the definition's default argv entirely when present; `${input_dir}` is the
materialization root and `${work_dir}` the private working directory. The
child environment is exactly the approval's `env_allowlist` resolved from the
parent, nothing else; a server that needs `HOME`, `PATH` or a toolchain
variable needs it listed.

| Definition | Languages | Default argv | Root markers |
|---|---|---|---|
| `gopls` | go | `serve` | `go.mod`, `go.work` |
| `rust-analyzer` | rust | | `Cargo.toml` |
| `pyright` | python | `--stdio` | `pyproject.toml`, `pyrightconfig.json`, `setup.py`, `requirements.txt` |
| `typescript-language-server` | typescript, tsx, javascript | `--stdio` | `tsconfig.json`, `jsconfig.json`, `package.json` |
| `clangd` | c, cpp | | `compile_commands.json`, `compile_flags.txt`, `.clangd`, `CMakeLists.txt` |
| `jdtls` | java | `-data ${work_dir}` | `pom.xml`, `build.gradle`, `build.gradle.kts` |

**Verification status.** `gopls` v0.23.0 was exercised end to end in an ad-hoc
run on the development machine (recorded in the Task 10 report: definition,
references, document and workspace symbols, call hierarchy, standard-library
locations excluded, idle stop). The other five profiles are implemented from
their documented stdio invocations and are **unverified against a real
tool**; `pyright`'s executable is the `pyright-langserver` script, and
`jdtls` launcher scripts vary by distribution. Approving one is the
operator's statement that the pinned executable behaves as an LSP server.

At start the executable is checksummed (compared when the approval pins a
`checksum`, always recorded in the input digest) and the version the server
reports in `serverInfo.version` is matched against `version_constraint` as a
version token: `0.23` accepts `v0.23.1` and gopls's JSON build description
containing `"Version":"v0.23.0"`, and rejects `10.23`. A server that reports
no version, or another one, is shut down: `CTX_TRUST_REQUIRED`. Detection by
`--version` is execution and is not performed.

## Process and stream wiring

Servers run through the shared `internal/process` runner and nothing else:
absolute executable, literal argv, allowlisted environment, private working
directory, process-group termination. The runner's `Spec` has a one-shot
`Stdin io.Reader` and a caller `Stdout io.Writer`; the overlay wires a
bidirectional stream over them with two `io.Pipe`s and keeps `Runner.Run` in
a goroutine for the server's lifetime. The JSON-RPC client itself speaks over
an `io.ReadWriter`, so the wiring is one struct.

Bounds, all finite:

| Bound | Source | Effect when hit |
|---|---|---|
| concurrent servers | `providers.lsp.max_servers` (default 1) | an idle server is stopped to make room; otherwise `CTX_RESOURCE_LIMIT` |
| in-flight requests per server | `providers.lsp.max_outstanding_requests` | callers wait for a slot under their context |
| one request | `providers.lsp.request_timeout` | `$/cancelRequest` sent, `CTX_PROVIDER_TIMEOUT` |
| idle server | `providers.lsp.idle_ttl` | shutdown/exit after the last overlay closes |
| server lifetime | the approval's `timeout` | the runner terminates the tree |
| materialized bytes, cached pinned bytes, bytes sent, bytes received | `Options.MaxOverlayBytes` (default 512 MiB) | materialization refused; cache evicts LRU; the server is failed |
| one message | `Options.MaxFrameBytes` (default 8 MiB), checked before allocation | the connection is failed |
| header block, header line, JSON depth | 8 lines, 1 KiB, 64 levels | protocol error, connection failed |
| documents held open on the server | 16 | `didClose` of the least recently opened |
| handshake, shutdown | `Options.StartTimeout` (60 s), `Options.StopTimeout` (5 s) | start fails; the runner forces the stop |

Stderr is a server's log and is discarded, not retained or logged (Section
22); its bound still terminates a server that floods it.

## Protocol behaviour

- **Lifecycle.** `initialize` with the client's capabilities, `initialized`,
  then requests; `shutdown`, `exit`, close stdin, wait for the tree, force
  through the runner if it stays. The materialization is removed once the
  tree is reaped, on every path.
- **Out-of-order responses** are matched by integer id. A response to an id
  the client has already abandoned is dropped.
- **Cancellation.** When a call's context ends the client sends
  `$/cancelRequest`, forgets the id and returns `CTX_CANCELED` (or
  `CTX_PROVIDER_TIMEOUT` for a deadline). The connection stays usable.
- **Server-initiated requests** are handled by explicit policy:
  `workspace/configuration` is answered with `null` per item (at most 64
  items) — codectx supplies no settings; **everything else** is answered with
  a JSON-RPC MethodNotFound error, `workspace/applyEdit` above all. The
  client never applies an edit, never runs a command
  (`workspace/executeCommand` is never sent) and never opens a URI on a
  server's behalf. Notifications (`window/logMessage`,
  `textDocument/publishDiagnostics`, `$/progress`) are read and dropped.
- **Position encoding.** The client offers `utf-8`, `utf-32`, `utf-16` in
  that order; the server's `positionEncoding` (default `utf-16`) is used for
  every coordinate in both directions. A choice the client did not offer is
  a protocol error. Conversion is the shared `internal/source` cursor over
  the **pinned bytes read from the verified snapshot view**, never the
  materialized copy the server may have written to and never the live
  checkout: a query byte offset becomes a line and a column counted in the
  negotiated units; a returned range becomes a half-open byte range through
  `Cursor.SourceRange`, which rejects a column inside a code point, past the
  line or past the file.
- **Document synchronization.** Before a positioned request the queried
  document is `didOpen`ed once with its pinned text and manifest
  `languageId` (`tsx` maps to `typescriptreact`; JSX files carry the
  manifest's `javascript`). The bytes never change while the snapshot is
  pinned, so `didChange` is never sent. A file that is not UTF-8 text has no
  addressable positions: `CTX_ARGUMENT_INVALID`. Servers whose
  `textDocumentSync` is `None` are not sent open/close.
- **URIs.** Only a `file:` URI without authority is a location. It is mapped
  back under the materialization root to a snapshot path and then to a
  `FileID`; the bytes come from the view. A `file:` URI outside the root (a
  standard library, a module cache) or naming a path the snapshot does not
  hold is **excluded** and counted in `Result.Excluded`, never opened. Any
  other scheme, an authority, or a relative path is
  `CTX_PROVIDER_OUTPUT_INVALID` — the client never told the server about
  such a resource.
- **Result validation.** A range that does not describe the pinned bytes
  fails the whole call with `CTX_PROVIDER_OUTPUT_INVALID` rather than
  dropping the item: a wrong position is a wrong source attribution and must
  not pass quietly.
- **Unsupported methods.** An operation the server did not advertise in its
  capabilities is `CTX_PROVIDER_UNAVAILABLE` with `reason=unsupported` and no
  request is sent. A server MethodNotFound is reported the same way; another
  server error is unavailable with the server's bounded code and message.
  Nothing is ever substituted from the canonical index here — routing to
  canonical facts is the facade's decision (Section 11.6), made explicitly.
- **Pages.** Every operation takes a `limit` (0 = `model.MaxPageItems`,
  above it invalid). Items beyond the limit set `Result.Truncated`; there is
  no cursor because the results are ephemeral and the server owns the order.

## Typed surface

```go
mgr, _ := lsp.New(lsp.Options{Runner: runner, DataDir: dataDir, MaxServers: cfg.Providers.LSP.MaxServers, ...})
profile, err := lsp.Trusted(cfg, "gopls")          // CTX_TRUST_REQUIRED without approval
ov, err := mgr.Open(ctx, view, profile)            // starts lazily, shares a running server
defer ov.Close()                                   // last close starts the idle TTL

ov.Capabilities()                                  // what the server advertised
ov.Binding()                                       // model.OverlayBinding: lsp:<name>, version, input digest
ov.Definition(ctx, lsp.At{File: id, Byte: off}, limit)      // Result[Location]
ov.TypeDefinition / Implementations / References(ctx, at, includeDeclaration, limit)
ov.DocumentSymbols(ctx, id, limit)                 // Result[Symbol], hierarchical answers flattened with Container
ov.WorkspaceSymbols(ctx, query, limit)
ov.PrepareCallHierarchy(ctx, at, limit)            // Result[CallItem]
ov.IncomingCalls / OutgoingCalls(ctx, item, limit) // Result[Call]: peer item plus validated call sites
```

`Location` carries `FileID`, path, content hash and a byte-authoritative
`model.SourceRange` (plus the identifier `Selection` for definition links
and symbols). `Result.Overlay` is the `model.OverlayBinding` every public
result must expose when `semantic_source=lsp`; `ProviderID` is
`lsp:<profile>`, `ProviderVersion` the bounded server-reported version and
`InputDigest` `H("lsp-overlay-v1", snapshot id, manifest hash, profile, executable,
checksum, reported version, encoding, argv...)`.

LSP `SymbolKind` maps onto Section 9.2 as: File→file, Module→module,
Namespace→namespace, Package→package, Class→class, Method and
Constructor→method, Property, Field and Event→field, Enum→enum,
Interface→interface, Function and Operator→function, Constant and
EnumMember→constant, Struct→struct; Variable, the value kinds (String …
Null), TypeParameter and anything unknown→variable.

## Failure

Any transport error, protocol violation, frame over its bound, process exit
or lifetime cap **fails the server**: every pending call is released with the
typed reason, the process tree is terminated through the runner, the
materialization is removed once the tree is reaped, and the manager forgets
the server so the next `Open` starts a fresh one. Overlays still holding the
failed server answer `CTX_PROVIDER_UNAVAILABLE` naming the cause. No
canonical fact is touched, because none is ever written here.

Detection of an unexpected exit is bounded by the runner's stdin handling:
the runner's input pump parks on the client's stdin reader after the child
dies and `Run` returns after twice the grace (`Options.StopTimeout`); the
Task 10 report names the shared change that would make it immediate.

## Consumers

The query facade (Task 17) routes `semantic_source=lsp` requests here with
the caller's `profile`, maps `Location`/`Symbol`/`Call` onto the overlay-aware
`model.Node` and `model.ReferenceOccurrence` shapes and sets
`QueryMeta.Overlay`. Context compilation always requests canonical mode and
never consults this package.
