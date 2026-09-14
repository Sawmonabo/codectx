# codectx

Codebase intelligence for software-development agents: a Go CLI and MCP server
that index a Git repository, keep the exact indexed bytes, and answer bounded
structural queries.

**Status: pre-release.** `codectx version`, `codectx tools`, `codectx index`,
`codectx refresh`, `codectx status`, `codectx watch`, `codectx search` and
`codectx symbol` exist today; the other commands below land in later tasks and
are shown as the target contract.

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
codectx tools prefetch --for-repo .   # only what this repository's root selects
codectx tools verify            # rehash the store against the lock
codectx tools gc                # drop versions this binary no longer pins
```

`docs/toolchain.md` has the pinned set, the offline and mirror settings, and the
per-platform matrix that proves each payload runs.

## Discovery

`codectx search` and `codectx symbol` answer over exactly one generation of the
index, pinned for the whole request:

```bash
codectx search "retry backoff" --repo .            # ranked hits across every tier
codectx search Authorize --kind method --limit 20  # filter by node kind
codectx search Authorize --language go             # filter by language
codectx symbol PaymentService.Authorize --repo .   # every candidate for a name
```

`search` runs five retrieval tiers -- exact path, exact qualified name,
qualified-name prefix, exact name, and generation-local BM25 lexical retrieval
-- and ranks them by tier first, then by an integer score, so the same query
over the same generation always returns the same page. Hits carry the matched
entity, its path and its source range, never source bodies.

`symbol` resolves a name, qualified name or canonical node ID to every node
that matches. An ambiguous name returns all the candidates rather than silently
picking the first.

Both take `--repo`, `--generation`, `--limit`, `--cursor`, `--timeout` and
`--json`; `search` additionally takes repeatable `--kind` and `--language`. A
page prints a continuation token as `next`: pass it back with
`--cursor` to read the following page from the generation the first page was
read from, which is why `--cursor` and `--generation` cannot be combined. A
tampered, expired or foreign token is refused with `CTX_CURSOR_INVALID`
(exit 8) rather than answered from a different question.

If only the bundled structural providers are available the commands still work
and report their precision; SCIP, LSP or dependence-engine analysis enriches the
same graph and query APIs without changing the agent integration.

`codectx refs`, `callers`, `callees`, `path` and `impact` answer bounded
structural questions about the indexed generation; `docs/queries.md` has their
arguments, budgets and the configuration keys they read.

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
