# Contributing

codectx is greenfield. There is no released version to stay compatible with, so
changes are made **in place**: no migration layers, no transitional shims, no
legacy paths kept alive. Remove the old path in the same change that replaces
it.

## The pinned toolchain

Go `1.27.1`, exactly. `go.mod` declares `go 1.27` with `toolchain go1.27.1`, and
CI sets `GOTOOLCHAIN=go1.27.1` so it never downloads a different one. Do not
bump either without also updating the release workflow and
[docs/toolchain.md](docs/toolchain.md).

Tree-sitter's official bindings are native code, so a build needs a working C
toolchain. Release builds must not claim `CGO_ENABLED=0` portability — SQLite's
CGo-free driver does not remove Tree-sitter's native requirement.

Every dependency is pinned to an exact version. No `latest`, and nothing is
downloaded at runtime that the embedded tool lock does not name with a URL and a
SHA-256.

## Before you change anything: the read gate

This is the single rule most often skipped, and it is binding.

Grep and ripgrep are for **discovery and navigation only**. Search results are
not a substitute for understanding the code. Before any architectural,
behavioural or implementation decision: open and fully read the files you intend
to change, then trace their callers, contracts, state ownership, dependencies
and integration boundaries.

Before adding anything, check whether an existing shared utility, helper or
abstraction already solves the problem, and reuse it. When duplicate behaviour
starts to emerge, consolidate it rather than letting two implementations drift.

Use the least clear, production-quality code that does the job. No speculative
infrastructure, no needless indirection, no premature abstraction, no
placeholder that a later change is supposed to fill in.

## The verification set

Run all of this before you open a pull request. CI runs the same checks, plus a
build.

```sh
go mod tidy && go mod verify
gofmt -l .            # must print nothing
go vet ./...
git diff --check
go test ./... -count=1
go test -race ./... -count=1
go build -trimpath -o ./bin/codectx ./cmd/codectx
```

CI additionally enforces the **single network boundary**: of the packages in
`./cmd/codectx`'s dependency graph, only `internal/toolchain` may import
`net/http`. It checks the built dependency graph, not a grep, so a new importer
fails the build even if it is reached indirectly. If your change needs to make a
network call, it belongs behind `internal/toolchain` or it does not belong in
the product path.

Performance numbers are measured on **uninstrumented release builds**. Race and
coverage builds are correctness runs, never measurements. A change over an
absolute budget in Section 23.2 fails the release gate; a regression over 10%
needs an investigation and an explanation, not a rewritten benchmark.

## Test policy

**Tests exist to protect critical invariants, not to raise a coverage number.**
A test is justified only when its silent breakage would corrupt data, serve the
wrong source bytes, leak unsealed facts, break determinism, grant false
readiness, or let a capability reduction pass as an optimization.

Do not add tests for:

- getters, enum spelling repeated at every layer, trivial forwarding, or
  constructor wiring;
- framework or standard-library behaviour;
- a second golden output of the same data through another adapter;
- "coverage".

Prefer extending one existing table or fixture test over adding a file. Every
test must name — in its name or in a comment — the failure mode it protects, and
every new assertion should be **mutation-proved**: make the one-line change that
should break it, confirm the test fails, revert, confirm it passes.

A test that does not meet this bar is a defect. It is reported in review and
removed. Deleting a redundant test is a legitimate change on its own.

## Pull requests

- One coherent change per pull request. Say what it changes and what you ran to
  verify it; paste the output rather than asserting a pass.
- No known in-scope defect, dead consumer, unreachable path, placeholder
  provider or unbounded operation may remain. Every new function needs a caller,
  every new field a writer and a reader, every configuration key you add must be
  read somewhere.
- Help text, doc comments and `docs/` must describe what the code now does.
  A command's documentation and its registration in `internal/cli/root.go` are
  checked against each other.
- Error codes come from the `CTX_*` families in
  [docs/implementation-plan.md](docs/implementation-plan.md); do not invent one.
- Never log source bodies, task text, secrets, inherited environment, SQL text
  or a private absolute path — including in an example or a pasted proof.
- Stage explicitly. Commits are made with the repository's configured identity.

## Reporting a security issue

Do not open a public issue. Follow [SECURITY.md](SECURITY.md).

## Licence

codectx is Apache-2.0. By contributing you agree your contribution is licensed
under the same terms. A change that adds or updates a redistributed analyzer,
runtime or grammar must regenerate `THIRD_PARTY_LICENSES.md` from the tool lock
rather than hand-editing it.
