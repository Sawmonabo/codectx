# codectx

Local-first codebase intelligence for software-development agents: a Go CLI and
MCP server that index a local Git repository, keep the exact indexed bytes, and
answer bounded structural queries offline.

**Status: pre-release.** Only `codectx version` exists today; the commands below
land in later tasks and are shown as the target contract.

## Quick start

```bash
go build -trimpath -o ./bin/codectx ./cmd/codectx

codectx index .
codectx context plan --task "Add retry semantics to PaymentService.Authorize" --phase verify --actor lead-session-1
codectx mcp serve --repo .
```

No API key, no hosted service and no mandatory network access at runtime. If
only the bundled structural providers are available the commands still work and
report their precision; installed SCIP, LSP or Joern tooling enriches the same
graph and query APIs without changing the agent integration.

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
  -ldflags "-X github.com/codectx-project/codectx/internal/model.version=$VERSION \
            -X github.com/codectx-project/codectx/internal/model.commit=$(git rev-parse HEAD)" \
  -o ./bin/codectx ./cmd/codectx
```

## Documentation and licensing

The full specification is `docs/implementation-plan.md`. codectx is Apache-2.0
licensed; see `LICENSE`, `NOTICE` and `THIRD_PARTY_LICENSES.md`.
