# LSP snapshot-qualified working-tree overlay

`internal/provider/lsp` is the Section 11.5 query overlay: on-demand
definition, references, implementations, type definition, document and
workspace symbols and call hierarchy, answered by a pinned local language
server running against a **private materialization of the pinned snapshot**.
It is not a `provider.Provider`. It writes nothing to storage, mints no
identities and emits no facts through a sink; every answer is ephemeral,
labelled with the server, its version and an input digest, and carries
`language_server` precision (`lsp.Precision`) and `semantic_source=lsp`
(`lsp.SemanticSource`). When the server stops the answers are simply gone;
canonical generations never change.

## What runs, and why it is allowed to

A `Definition` is what this build knows about a supported server: name — which
is also the name of its entry in the embedded tool lock — manifest language
tags, the stdio argument array, the environment allowlist, the budgets and the
root markers. `Definitions()` lists the six of Section 11.5. A definition
**authorizes nothing**. `Definition.Detect(root)` reports which root markers
exist, reading metadata through the confined `workspace.Root` only; it is a
hint for choosing among the supported servers and never a reason to execute.

A `Profile` is runnable, and `Resolve(ctx, resolver, cfg, name)` is its only
constructor:

| Condition | Result |
|---|---|
| `providers.lsp.enabled = false` | `CTX_PROVIDER_UNAVAILABLE` |
| `name` is not one of the six definitions | `CTX_ARGUMENT_INVALID` |
| no toolchain resolver | `CTX_PROVIDER_UNAVAILABLE` |
| the payload is not installed and `tools.offline` is set | `CTX_TOOL_OFFLINE` |
| the lock carries no payload for this platform | `CTX_TOOL_UNSUPPORTED_PLATFORM` |
| the store's payload does not match the lock | `CTX_TOOL_CORRUPT` |
| a `[tools.override.<name>]` does not verify | `CTX_TOOL_OVERRIDE_INVALID` |
| the fetch failed or the bytes disagree | `CTX_TOOL_FETCH_FAILED`, `CTX_TOOL_DIGEST_MISMATCH` |

`enabled = "auto"` therefore means "start the pinned payload for this
language"; nothing found on `PATH` is ever started and there is no
`[analyzers.<name>]` approval to write (Section 20.2 — trust is the lock). The
argument array, the environment allowlist, the budgets and the lifetime are
constants of this build; `${input_dir}` is the materialization root and
`${work_dir}` the server's private working directory
`<data_dir>/lsp/<server>/<payload identity>/`, which the overlay creates before
the server starts. The last component is the digest half of the resolved
payload's `Tool.Fingerprint()`: what the directory holds is derived from the
payload and is never rewritten, so a new payload gets a new directory. The
previous one is **left in place** — nothing reclaims anything under
`<data_dir>/lsp` today — so a machine keeps one tree per payload version it has
been pinned to, which is the cost of never rewriting a configuration underneath
a running server. (The digest rather
than the rendered fingerprint because this is a path component, and the
rendered form carries the version verbatim — an override's version is whatever
the user typed.)
It is a real working directory, not a scratch path — `jdtls` is started with
`-data ${work_dir}/data` **and** `-configuration ${work_dir}/config`, and
writes its workspace state and its whole Equinox configuration there; the
materialization is the server's read-only input and is never its data
directory. The child's environment is exactly the allowlisted variables the
parent has plus the variables the payload needs, and the payload's come last so
a host `JAVA_HOME` can never shadow the managed JDK.

| Definition | Languages | Arguments after the launcher | Environment allowlist | Root markers |
|---|---|---|---|---|
| `gopls` | go | `serve` | `PATH HOME GOPATH GOCACHE GOMODCACHE GOFLAGS GOPROXY GOPRIVATE` | `go.mod`, `go.work` |
| `rust-analyzer` | rust | | `PATH HOME CARGO_HOME RUSTUP_HOME` | `Cargo.toml` |
| `pyright` | python | `--stdio` | `PATH HOME` | `pyproject.toml`, `pyrightconfig.json`, `setup.py`, `requirements.txt` |
| `typescript-language-server` | typescript, tsx, javascript | `--stdio` | `PATH HOME` | `tsconfig.json`, `jsconfig.json`, `package.json` |
| `clangd` | c, cpp | | `PATH HOME` | `compile_commands.json`, `compile_flags.txt`, `.clangd`, `CMakeLists.txt` |
| `jdtls` | java | `-configuration ${work_dir}/config -data ${work_dir}/data` | `PATH HOME` | `pom.xml`, `build.gradle`, `build.gradle.kts` |

The launcher itself comes from the lock: `gopls`, `clangd` and `rust-analyzer`
run as themselves; `pyright` and `typescript-language-server` run as
`<managed node> <pinned entry>`; `jdtls` runs as
`<managed jdk>/bin/java -jar <pinned launcher jar>` with `JAVA_HOME` set.

**`jdtls` needs two things the table above cannot express, and both are
required for it to start at all.** The first is `Definition.RuntimeArgs`:
arguments of the *runtime*, placed between the managed JDK and `-jar`, because
a JVM option after `-jar` is an application argument and the Equinox launcher
would never boot. They are the application identity Eclipse expects
(`-Declipse.application=org.eclipse.jdt.ls.core.id1`,
`-Dosgi.bundles.defaultStartLevel=4`,
`-Declipse.product=org.eclipse.jdt.ls.core.product`) and the module openings
jdt.ls reflects through (`--add-modules=ALL-SYSTEM`, `--add-opens
java.base/java.util=ALL-UNNAMED`, `--add-opens
java.base/java.lang=ALL-UNNAMED`). Setting them on a *pinned* payload that is
not runtime-hosted is a product defect and `Resolve` refuses it with
`CTX_INTERNAL`. The second is the configuration directory: Equinox **writes**
into whatever `-configuration` names, so `${work_dir}/config` is a private copy
seeded once from the payload's own `config_linux`/`config_mac`/`config_win`
(`config_linux_arm`/`config_mac_arm` on arm64). Seeded once is safe only
because `${work_dir}` is per payload identity: the configuration names bundle
jars by exact version, so a directory shared across payloads would boot a
re-pinned `jdtls` against the previous payload's bundle list — reproduced, the
server dies at startup writing `An error has occurred. See the log file` to
stdout, and nothing in the product ever rewrites the directory. A new payload
seeds a new one. Pointed at the payload's own,
three starts left four new paths inside the store's published,
digest-identified version directory while `codectx tools verify` still reported
`14 installed`, exit 0 — verify rehashes the pinned entry, not the payload
tree.

Both specials belong to the pinned payload alone. A `[tools.override.jdtls]`
is run directly, with no managed runtime composed around it and no payload tree
to seed from, so the runtime arguments are dropped from its argv (which changes
`input_digest`, correctly: a different invocation is a different question) and
no configuration is copied. An override replaces the binary and never the
invocation; a wrapper script that needs JVM options passes its own.

**Verification status.** All six servers were run through `lsp.Resolve` +
`Manager.Open` on `linux/amd64` against a fixture of their own language, over
the real lock and store, and each completed initialize, published an overlay
binding that validates, advertised its capabilities and shut down cleanly:

| Server | Reported version | Bound `ProviderVersion` | Capabilities |
|---|---|---|---|
| `gopls` | `v0.23.0` | `v0.23.0` | all seven |
| `rust-analyzer` | `0.3.3016-standalone` | same | all seven |
| `clangd` | `clangd version 22.1.6 …` | same | all seven |
| `pyright` | **none** | `1.1.414` (pinned) | all but implementations |
| `typescript-language-server` | **none** | `6.0.0` (pinned) | all seven |
| `jdtls` | `1.61.0-SNAPSHOT` | same | all seven |

`pyright` and `typescript-language-server` answer `initialize` with no
`serverInfo` object at all (measured, both through the product and with a raw
`initialize` outside it). `OverlayBinding.Validate` requires a non-empty
`ProviderVersion`, so the overlay used to fail for python, typescript, tsx and
javascript with `CTX_ARGUMENT_INVALID: overlay.provider_version is required`.
The label now falls back to the version of the payload the lock pinned, which
is never empty and is the honest identity of a server that declines to name
itself; the **reported** string keeps feeding the input digest unchanged, so a
payload that starts reporting a version later is still a different question.
The jdtls version directory in the store is byte-identical before and after a
start, and `codectx tools verify` exits 0 over it. The protocol itself —
out-of-order replies, all three position encodings, cancellation, hostile URIs,
unadvertised methods, a server-initiated edit, a server that reports no
`serverInfo`, shutdown and crash — is exercised in full against the fake
server.

At start nothing is re-hashed: `internal/toolchain` hashed the payload's entry
at resolution, and `Tool.Fingerprint()` — which commits to the pinned payload
digest and to the executables observed at that resolution — is folded into the
overlay's input digest, so a replaced payload is a different answer. The
version the server reports in `serverInfo.version` is recorded as provenance in
`OverlayBinding.ProviderVersion` and in the input digest, and is **not** matched
against a constraint: the lock's digest is what identifies the bytes, and a
server's printed version has no fixed relationship to the release it came from
(the `rust-analyzer` release tagged `2026-08-17.4` reports
`1.98.0 (88d9e12 2026-08-18)`). A constraint written to accept that accepts
anything. Detection by `--version` is execution and is not performed.

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
| server lifetime | the definition's own `Timeout` (1 h) | the runner terminates the tree |
| materialized bytes, one admitted file, bytes sent per rolling minute | `Options.MaxOverlayBytes` (default unlimited) | files that do not fit are named in the log and left out; a file over the bound is not admitted and the query answers about the rest; a sustained flood over the send budget fails the server |
| cached pinned bytes | `Options.MaxOverlayBytes` when set, otherwise `DefaultDocCacheBytes` (512 MiB) | cache evicts least-recently-used; eviction costs a re-read, never an answer |
| one inbound message | `Options.MaxFrameBytes` (default 8 MiB), checked against the declared `Content-Length` before the body buffer exists | the connection is failed |
| one outbound message | `Options.MaxFrameBytes`; `didOpen` refuses a document that cannot fit before the text is copied | `CTX_RESOURCE_LIMIT` (`limit=max_frame_bytes`); nothing is written, so the connection stays usable — a request reports it to its caller, an answer to a server-initiated request is dropped and the writer goroutine continues |
| header block, header line, JSON depth | 8 lines, 1 KiB, 64 levels — the line bound is applied to the chunk the reader holds before it is accumulated | protocol error, connection failed |
| queued answers to server-initiated requests | 4 | the reply is dropped; the reader never blocks on a write |
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
  `conn.write` itself is not context-aware, so a call can wait on the writer
  lock behind a frame the server has not yet consumed; that wait is bounded by
  the server's own progress and ultimately by the approval's `timeout`
  terminating the tree, not by `providers.lsp.request_timeout`.
- **Server-initiated requests** are handled by explicit policy:
  `workspace/configuration` is answered with `null` per item (at most 64
  items) — codectx supplies no settings; **everything else** is answered with
  a JSON-RPC MethodNotFound error, `workspace/applyEdit` above all. The
  client never applies an edit, never runs a command
  (`workspace/executeCommand` is never sent) and never opens a URI on a
  server's behalf. Notifications (`window/logMessage`,
  `textDocument/publishDiagnostics`, `$/progress`) are read and dropped, as
  is a response the server could not attribute to a request (`"id": null`).
  The answer is **queued** (at most four) for one dedicated writer goroutine:
  the goroutine that reads the server's output never writes, because a write
  parks on the server's stdin while the server is parked writing stdout that
  only the reader drains. A full queue drops the reply rather than blocking,
  and so does a reply the frame bound refuses — its id and method name come
  from the server, so one long-named server request must not be able to stop
  every later answer. The writer exits only once the connection is failed.
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
  addressable positions: `CTX_ARGUMENT_INVALID`. A file too large to carry in
  one protocol message is refused with `CTX_RESOURCE_LIMIT` before its text is
  copied anywhere. Servers whose `textDocumentSync` is `None` are not sent
  open/close.
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
profile, err := lsp.Resolve(ctx, resolver, cfg, "gopls")     // CTX_TOOL_* when the payload does not resolve
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

`Manager.Close` returns once **every** process tree is reaped and every
materialization removed, including the trees of servers that failed and were
forgotten: the manager keeps a server on a `live` set from the moment its
runner goroutine starts until its exit path has removed the materialization,
and `Close` stops everything on that set as well as everything still holding a
slot. `Servers()` counts only the slots, not the set, so a crashed server that
has already been forgotten does not keep it above zero.

Detection of an unexpected exit is bounded by the runner's stdin handling:
the runner's input pump parks on the client's stdin reader after the child
dies and `Run` returns after twice the grace (`Options.StopTimeout`); the
Task 10 report names the shared change that would make it immediate.

## Documented residuals

These are deliberate and bounded, not defects; each is named here so a reader
does not have to rediscover it.

- **A server's lifetime is the approval's `timeout`.** There is no separate
  lifetime setting: the value that bounds one analyzer run bounds a whole
  language server here, and when it elapses the runner terminates the tree and
  the next `Open` starts a fresh server. An operator approving a server for
  interactive use approves a `timeout` of that length.
- **`Options.MaxOverlayBytes` is set from `providers.lsp.max_overlay_bytes`**
  (default unlimited; a value you set is validated to be at least
  `resources.max_source_response_bytes`). When set it bounds, separately, the
  materialized snapshot, admission of one file to the pinned coordinate cache,
  and the bytes sent to the server in one rolling minute. The first two report
  what they left out and answer about the rest; only a sustained flood over the
  send budget fails a server, because a truncated frame would leave it
  mid-message. Unlimited leaves the server's own streams unbounded — its stdout
  is a framed protocol whose reader paces it, and its stderr is discarded — and
  the pinned cache keeps a finite ceiling of its own so the overlay's peak stays
  flat rather than becoming a function of the repository.
- **`jdtls` writes into `work_dir` by design.** Its default argv is
  `-data ${work_dir}`, so the private working directory is also its workspace
  data directory and it accumulates state there across runs. Approving
  `jdtls` means approving that write. A payload upgrade re-creates that
  workspace index, because the work directory is per payload identity; that is
  the safe direction, and the previous payload's tree is left on disk rather
  than rewritten under a running server — nothing reclaims it yet. No snapshot byte and no repository file
  is ever written: the materialization is read-only input and the checkout is
  never touched.
- **Unexpected-exit detection is delayed** by twice the grace, as described
  above.

## Consumers

The query facade (Task 17) routes `semantic_source=lsp` requests here with
the caller's `profile`, maps `Location`/`Symbol`/`Call` onto the overlay-aware
`model.Node` and `model.ReferenceOccurrence` shapes and sets
`QueryMeta.Overlay`. Context compilation always requests canonical mode and
never consults this package.
