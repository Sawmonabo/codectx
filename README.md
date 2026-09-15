# codectx

Codebase intelligence for software-development agents: a Go CLI and MCP server
that index a Git repository, keep the exact indexed bytes, and answer bounded
structural queries — locally, offline, with no AI service and no API key.

**Status: pre-release.** Every command listed under [Commands](#commands) is
implemented and wired into the command tree. What is still moving is the
release surface — published archives, the SBOM and the signed checksum file
land with the first tagged release, so the install one-liners below describe
that release rather than something you can run against a published tag yet.
Build from source until then.

## Quick start

codectx owns every analyzer it runs and every runtime those analyzers need — no
Node, no npm, no JDK and no language server of your own.

Precise indexing of Go, Rust and Python additionally uses that language's own
toolchain (`go`, `cargo`, `python3`/`pip3`) the way any build does, because the
indexer loads the project model through it. Where one is absent that language
falls back to the structural and dependence providers, which need nothing on the
host; no other language depends on anything you install.

### Install the latest release

```sh
curl -fsSL https://github.com/Sawmonabo/codectx/releases/latest/download/install.sh | sh
```

`install.sh` resolves `latest` by default, downloads the archive for your
platform, verifies its SHA-256 against the release's `checksums.txt` **before it
extracts anything**, and installs into `~/.local/bin`. It fetches nothing that
`checksums.txt` does not name.

### Install a pinned version

Pin the release in CI and anywhere a reproducible install matters:

```sh
curl -fsSL https://github.com/Sawmonabo/codectx/releases/download/v1.2.3/install.sh \
  | sh -s -- --version v1.2.3 --prefix /usr/local
```

`--bundle` installs the bundle archive instead — the same binary plus the managed
tool store already populated for your platform. The script installs the store but
does not edit your configuration: it prints the `[tools] offline` and
`[tools] cache_dir` block to add, and until you add it the binary resolves tools
under the per-workspace default store and may still fetch on first use. See
[docs/toolchain.md](docs/toolchain.md#offline-hosts-and-bundle-archives).

### Download, inspect, then run

If you would rather not pipe a script into a shell, do the same three steps by
hand:

```sh
V=v1.2.3
BASE=https://github.com/Sawmonabo/codectx/releases/download/$V
curl -fsSLO $BASE/codectx_${V#v}_linux_amd64.tar.gz
curl -fsSLO $BASE/checksums.txt
sha256sum --ignore-missing --check checksums.txt   # macOS: shasum -a 256 -c checksums.txt
tar -xzf codectx_${V#v}_linux_amd64.tar.gz
./codectx version
```

`sbom.spdx.json` and `THIRD_PARTY_LICENSES.md` are published beside them, and
`install.sh` itself is a release asset you can read before you run it.

### Windows

Windows ships a zip, so the same three steps are PowerShell rather than `sh`.
There is no `install.sh` path on Windows; this is the supported install for both
Windows targets.

```powershell
$V = "v1.2.3"
$base = "https://github.com/Sawmonabo/codectx/releases/download/$V"
Invoke-WebRequest "$base/codectx_$($V.TrimStart('v'))_windows_amd64.zip" -OutFile codectx.zip
Invoke-WebRequest "$base/checksums.txt" -OutFile checksums.txt
# Compare this hash against the matching line of checksums.txt before extracting.
Get-FileHash -Algorithm SHA256 codectx.zip
Expand-Archive codectx.zip -DestinationPath "$env:LOCALAPPDATA\codectx"
$env:Path += ";$env:LOCALAPPDATA\codectx"   # persist with setx to keep it
codectx version
```

Substitute `windows_arm64` for an arm64 host. Under WSL, use the Linux
instructions above — WSL is served by the Linux build.

### Build from source

Requires the pinned toolchain `go1.27.1`. Release builds set the version
identity without embedding a build timestamp:

```sh
go build -trimpath \
  -ldflags "-X github.com/Sawmonabo/codectx/internal/model.version=$VERSION \
            -X github.com/Sawmonabo/codectx/internal/model.commit=$(git rev-parse HEAD)" \
  -o ./bin/codectx ./cmd/codectx
```

Tree-sitter's official bindings are native code, so a source build needs a C
toolchain and cannot claim `CGO_ENABLED=0` portability.

### First run

```sh
codectx init .        # write .codectx.toml
codectx doctor .      # check the installation before trusting an answer
codectx index .
codectx mcp serve --repo .
```

## Commands

| Group | Commands |
|---|---|
| Installation and health | `version`, `init`, `doctor` |
| Managed toolchain | `tools status`, `tools prefetch`, `tools verify`, `tools gc` |
| Indexing | `index`, `refresh`, `status`, `watch` |
| Discovery | `search`, `symbol`, `repo-map` |
| Graph queries | `refs`, `callers`, `callees`, `path`, `impact` |
| Context sessions | `context plan`, `status`, `next`, `entries`, `include`, `read`, `acknowledge`, `waive`, `record`, `advance`, `capsule`, `close`, `export` |
| Agent integration | `mcp serve` |

`codectx doctor` runs the diagnostic checks — build and toolchain pins,
workspace and data-directory permissions, free space, schema, FTS and WAL, the
active generation pointer, freshness, sampled content blocks, grammar
availability and retention — each with its own state, reason code and
remediation. A metric this host cannot measure is reported as unavailable, never
as zero; [docs/operations.md](docs/operations.md) has the check list and what
each recovery code means.

`codectx repo-map` reports the bounded repository, module, package and directory
map of the pinned generation, continued by token rather than truncated — see
[docs/queries.md](docs/queries.md#the-repository-map--repo-map).

## The managed toolchain

Every external analyzer is pinned by an exact version, a per-platform URL and a
SHA-256 in a lock the binary embeds. Indexing installs what your repository
needs, verified against that lock before anything executes, into a user-private
store outside the repository. codectx itself looks nothing up on `PATH`; the
three host toolchains above are the analyzers' own, and
[docs/toolchain.md](docs/toolchain.md) lists them in one place.

`codectx tools` is the optional lifecycle surface for CI and offline hosts:

```sh
codectx tools status            # every pinned tool and what the store holds
codectx tools prefetch --all    # install ahead of time instead of on demand
codectx tools prefetch --for-repo .   # only what this repository's root selects
codectx tools verify            # rehash the store against the lock
codectx tools gc                # drop versions this binary no longer pins
```

Set `[tools] offline = true` to make every fetch a typed refusal that opens no
socket; a bundle archive needs nothing else.

## Discovery

`codectx search` and `codectx symbol` answer over exactly one generation of the
index, pinned for the whole request:

```sh
codectx search "retry backoff" --repo .            # ranked hits across every tier
codectx search Authorize --kind method --limit 20  # filter by node kind
codectx search Authorize --language go             # filter by language
codectx symbol PaymentService.Authorize --repo .   # every candidate for a name
```

`search` runs five retrieval tiers — exact path, exact qualified name,
qualified-name prefix, exact name, and generation-local BM25 lexical retrieval —
and ranks them by tier first, then by an integer score, so the same query over
the same generation always returns the same page. Hits carry the matched entity,
its path and its source range, never source bodies.

`symbol` resolves a name, qualified name or canonical node ID to every node that
matches. An ambiguous name returns all the candidates rather than silently
picking the first.

Both take `--repo`, `--generation`, `--limit`, `--cursor`, `--timeout` and
`--json`; `search` additionally takes repeatable `--kind` and `--language`. A
page prints a continuation token as `next`: pass it back with `--cursor` to read
the following page from the generation the first page was read from, which is
why `--cursor` and `--generation` cannot be combined. A tampered, expired or
foreign token is refused with `CTX_CURSOR_INVALID` (exit 8) rather than answered
from a different question.

If only the bundled structural providers are available the commands still work
and report their precision; SCIP, LSP or dependence-engine analysis enriches the
same graph and query APIs without changing the agent integration. What that
precision does and does not prove is set out in
[docs/providers.md](docs/providers.md) and
[docs/snapshots.md](docs/snapshots.md).

`codectx refs`, `callers`, `callees`, `path` and `impact` answer bounded
structural questions about the indexed generation;
[docs/queries.md](docs/queries.md) has their arguments, budgets and the
configuration keys they read.

## Context sessions

`codectx context` compiles a snapshot-pinned manifest for one task, then tracks
which actor actually read which bytes of it. A session seals only when every
required file has a delivery receipt confirmed by the actor that received it, or
an auditable waiver in its place — so "fully read" is evidence, not a claim.
[docs/context-sessions.md](docs/context-sessions.md) walks the whole flow.

```sh
codectx context plan --task "Add retry semantics to PaymentService.Authorize" \
  --phase sweep --actor lead-session-1
```

## Output contract

Every `--json` request emits one envelope on stdout with `schema_version`,
`command`, `ok`, `data`, `warnings` and `error`. Logs and human error text go to
stderr. Exit codes follow Section 18.2 of
[docs/implementation-plan.md](docs/implementation-plan.md):

```console
$ codectx version --json
{"schema_version":"1","command":"version","ok":true,"data":{...},"warnings":[],"error":null}
```

## Documentation

| Document | What it covers |
|---|---|
| [docs/operations.md](docs/operations.md) | Diagnosis, recovery codes, retention and the grace window, disk pressure, rebuilding |
| [docs/configuration.md](docs/configuration.md) | Every configuration key, its bounds and the cross-field rules |
| [docs/toolchain.md](docs/toolchain.md) | The pinned analyzer set, offline hosts and bundles, mirrors, the per-platform matrix |
| [docs/context-sessions.md](docs/context-sessions.md) | The strict actor workflow: receipts, review, waivers and the sealed capsule |
| [docs/queries.md](docs/queries.md) | `refs`, `callers`, `callees`, `path`, `impact` and `repo-map` |
| [docs/snapshots.md](docs/snapshots.md) | Snapshots, the content-addressed store and what provenance guarantees |
| [docs/storage.md](docs/storage.md) | Generations, delta imports and carry-over |
| [docs/providers.md](docs/providers.md) | The provider contract, and the per-provider pages beside it |
| [docs/threat-model.md](docs/threat-model.md) | What is defended, what is not, and the honest limitations |
| [docs/adr/](docs/adr/) | Architecture decision records: the decision, the alternatives weighed against it, and why it won |
| [SECURITY.md](SECURITY.md) | The operator-facing trust boundary and how to report an issue |
| [CONTRIBUTING.md](CONTRIBUTING.md) | The pinned toolchain, the verification set and the test policy |

The full specification is [docs/implementation-plan.md](docs/implementation-plan.md).

## Licensing

codectx is Apache-2.0 licensed; see `LICENSE`, `NOTICE` and
`THIRD_PARTY_LICENSES.md`, which records the version and license of every
redistributed analyzer, runtime and grammar.
