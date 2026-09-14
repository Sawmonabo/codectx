# codectx

Codebase intelligence for software-development agents: a Go CLI and MCP server
that index a Git repository, keep the exact indexed bytes, and answer bounded
structural queries.

**Status: pre-release.** `codectx version` and `codectx tools` exist today; the
other commands below land in later tasks and are shown as the target contract.

## Quick start

Install and go. codectx owns every analyzer it runs and every runtime those
analyzers need — no Node, no npm, no JDK and no language server of your own.

Precise indexing of Go, Rust and Python additionally uses that language's own
toolchain (`go`, `cargo`, `python3`/`pip3`) the way any build does, because the
indexer loads the project model through it. Where one is absent that language
falls back to the structural and dependence providers, which need nothing on the
host; no other language depends on anything you install.

```bash
go build -trimpath -o ./bin/codectx ./cmd/codectx

codectx index .
codectx context plan --task "Add retry semantics to PaymentService.Authorize" --phase verify --actor lead-session-1
codectx mcp serve --repo .
```

Every external analyzer is pinned by an exact version, a per-platform URL and a
SHA-256 in a lock the binary embeds. Indexing installs what your repository
needs, verified against that lock before anything executes, into a user-private
store outside the repository. codectx itself looks nothing up on `PATH`; the
three host toolchains above are the analyzers' own, and `docs/toolchain.md` lists
them in one place.

`codectx tools` is the optional lifecycle surface for CI and offline hosts:

```bash
codectx tools status            # every pinned tool and what the store holds
codectx tools prefetch --all    # install ahead of time instead of on demand
codectx tools verify            # rehash the store against the lock
codectx tools gc                # drop versions this binary no longer pins
```

`docs/toolchain.md` has the pinned set, the offline and mirror settings, and the
per-platform matrix that proves each payload runs.

If only the bundled structural providers are available the commands still work
and report their precision; SCIP, LSP or dependence-engine analysis enriches the
same graph and query APIs without changing the agent integration.

## Output contract

Every `--json` request emits one envelope on stdout with `schema_version`,
`command`, `ok`, `data`, `warnings` and `error`. Logs and human error text go to
stderr. Exit codes follow Section 18.2 of `docs/implementation-plan.md`:

```bash
$ codectx version --json
{"schema_version":"1","command":"version","ok":true,"data":{...},"warnings":[],"error":null}
```

## Building

Requires the pinned toolchain `go1.27.1`. Release builds set the version
identity without embedding a build timestamp:

```bash
go build -trimpath \
  -ldflags "-X github.com/Sawmonabo/codectx/internal/model.version=$VERSION \
            -X github.com/Sawmonabo/codectx/internal/model.commit=$(git rev-parse HEAD)" \
  -o ./bin/codectx ./cmd/codectx
```

## Documentation and licensing

The full specification is `docs/implementation-plan.md`. codectx is Apache-2.0
licensed; see `LICENSE`, `NOTICE` and `THIRD_PARTY_LICENSES.md`.
