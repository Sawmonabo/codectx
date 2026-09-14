# The managed analyzer toolchain: what is pinned and why

codectx owns every external analyzer it runs and every runtime those analyzers
need. Nothing is looked up on `PATH`, nothing is installed by the user, and
nothing executes that the shipped binary did not pin. This document describes
the pinned set and the provenance behind it (Section 11.7). The runtime
behaviour — store layout, resolution order, offline refusals, `codectx tools`
commands — belongs to `internal/toolchain`.

**The user needs no toolchain of their own.** Not Go, not Node, not npm, not a
JDK. Node comes from a pinned nodejs.org distribution and the JDK from a pinned
Eclipse Temurin build; the four Node-hosted analyzers ship as prebuilt
`node_modules` trees produced at release time by `npm ci --omit=dev`, so no
`npm install` ever runs on a user's machine; `gopls` is cross-compiled at
release time for all six targets. Installing codectx installs nothing else.

## The three artifacts

| Artifact | Produced by | Role |
|---|---|---|
| `internal/toolchain/tools.lock.json` | `internal/tools/toollock` | The embedded lock. One entry per tool with its version, license, upstream, runtime and a per-platform `{url, sha256, size, entry, entry_sha256, provenance}`. Data only. |
| `tools-v<n>` release assets | `internal/tools/toollock` | One deterministic `<tool>-<version>-<os>-<arch>.tar.gz` per platform, for the five tools that have no upstream binary at all. |
| Profiles (`internal/provider/...`) | Product code | The argv, trigger files, budgets, timeouts and env allowlist. A profile names a lock entry; it never names a command found on the host. |

A platform absent from a tool's `platforms` map means the tool is unavailable
there. That is reported as unavailability with a typed reason, never as a
failure.

## Hybrid hosting: pin upstream, host only what upstream lacks

A lock entry's URL points at **the upstream asset itself** whenever upstream
publishes a real binary for that platform, and at **an asset of our own
`tools-v<n>` release** only when it does not.

The security property is identical either way, and it comes from the digest,
not from the host. The lock pins the payload's SHA-256 and size; the runtime
verifies both before anything is extracted or executed, and refuses on the
first byte of disagreement. A tampered nodejs.org mirror and a tampered
GitHub release fail the same check. Re-hosting a byte-identical copy of an
upstream artifact adds no integrity and costs about 14 GB of storage, 14 GB of
upload per release and a second supply chain to keep honest, so it is not done.

What re-hosting *would* add is independence from upstream retention. That is a
real risk and it is accepted deliberately: an upstream that deletes an asset
makes that tool unavailable with a typed fetch error, which is recoverable by
cutting a new lock, and is not a correctness or integrity failure. Where
upstream publishes no binary at all, there is nothing to depend on and we
build and host the payload ourselves.

| Hosted on `tools-v1` (no upstream binary exists) | Pinned at the upstream URL |
|---|---|
| `gopls` — upstream ships no binaries; cross-built for all six | `node`, `jdk`, `joern`, `jdtls`, `clangd`, `scip-go`, `scip-java`, `scip-clang`, `rust-analyzer` |
| `scip-typescript`, `scip-python`, `typescript-language-server`, `pyright` — npm packages, prebuilt so the user never runs npm | |

The five hosted payloads are normalized deterministically: entries sorted by
path, one fixed modification time, uid/gid 0, no owner names, modes collapsed
to `0644`/`0755`, and a gzip header with no name and no timestamp. Packing the
same tree twice produces byte-identical bytes, so a regenerated release does
not invalidate a lock a shipped binary already carries. The packer refuses any
entry whose path or symlink target escapes the payload root, and refuses a
payload above the 2 GiB release-asset ceiling rather than discovering it
mid-upload.

Entry paths in the lock are relative to the archive **as upstream ships it** —
`node-v22.23.2-linux-x64/bin/node`, `joern-cli/joern-parse`,
`jdk-21.0.12.1+1/Contents/Home/bin/java` on macOS. Nothing is restripped or
repacked, so what the runtime extractor writes is exactly what upstream
published.

### Mirrors

Because the lock now names several hosts, `tools.mirror` replaces the scheme
and host of a payload URL and keeps the original host as the first path
segment:

```
https://nodejs.org/dist/v22.23.2/node-v22.23.2-linux-x64.tar.gz
  -> <mirror>/nodejs.org/dist/v22.23.2/node-v22.23.2-linux-x64.tar.gz
```

Every URL in the lock is therefore an absolute `https` URL with a stable host
and a path, which the lock's own validation enforces. The hosts in use are
`nodejs.org`, `github.com` and `download.eclipse.org`.

## The pinned set

| Tool | Version | Kind | Runtime | Languages | Platforms | Hosting |
|---|---|---|---|---|---|---|
| `node` | 22.23.2 (Jod LTS) | runtime | — | — | all six | upstream `nodejs.org/dist`, digest from `SHASUMS256.txt` |
| `jdk` | Temurin 21.0.12.1+1 | runtime | — | — | all six | upstream `adoptium/temurin21-binaries` release, published SHA-256 |
| `scip-go` | 0.2.7 | indexer | — | go | linux amd64/arm64, darwin arm64 | upstream, sidecar `.sha256` |
| `scip-typescript` | 0.4.0 | indexer | node | typescript, tsx, javascript | all six | hosted; `npm ci --omit=dev` at release time |
| `scip-python` | 0.6.6 | indexer | node | python | all six | hosted; `npm ci --omit=dev` at release time |
| `scip-java` | 0.13.1 | indexer | jdk | java | all six | upstream launcher jar, sidecar `.sha256` |
| `rust-analyzer` | 2026-08-17.4 | indexer (also the Rust server) | — | rust | all six | upstream; upstream publishes no digest |
| `scip-clang` | 0.4.0 | indexer | — | c, cpp | linux amd64, darwin arm64 | upstream; upstream publishes no digest |
| `gopls` | 0.23.0 | server | — | go | all six | hosted; cross-built, upstream ships no binaries |
| `typescript-language-server` | 6.0.0 | server | node | typescript, tsx, javascript | all six | hosted; with TypeScript 5.9.3 |
| `pyright` | 1.1.414 | server | node | python | all six | hosted; `npm ci --omit=dev` at release time |
| `clangd` | 22.1.6 | server | — | c, cpp | linux amd64, darwin amd64, windows amd64 | upstream; upstream publishes no digest |
| `jdtls` | 1.61.0 | server | jdk | java | all six | upstream Eclipse tarball, sidecar `.sha256` |
| `joern` | 4.0.627 | cpg | jdk | all nine | all six | upstream `joern-cli` archives, sidecar `.sha512` |

The engine name in the last row appears here, in the lock and in the license
inventory only. It never appears in a command, a config key, a provider id, an
evidence detail or a query result: the provider is `dependence`, so replacing
the engine is a backend swap rather than a product change.

### Notes on individual pins

- **`node` 22 rather than a newer line.** `typescript-language-server` 6.0.0
  declares `engines.node >= 22.22.2`, and 22 is the active LTS. Every
  node-hosted payload runs under this one managed runtime.
- **`typescript` 5.9.3 beside the language server.** The server drives
  `lib/tsserver.js`; the 7.x `typescript` package is the native port and no
  longer ships it. Pinning 5.9.3 keeps the server's own protocol path intact.
- **`scip-java` and `jdtls` are platform-independent** and are pinned once,
  under all six platform keys, at the same upstream URL. `scip-java`'s upstream
  asset carries a shell preamble ahead of the jar; `java -jar` tolerates it,
  which is what lets Windows run it under the managed JDK.
- **`gopls` is cross-built.** Upstream publishes no binaries at all. The
  payloads are built with `go install <pkg>@<version>` under `CGO_ENABLED=0`
  and `-trimpath`, outside the product's own module graph, and hosted on the
  tools release.
- **`scip-go` covers three platforms.** Upstream builds linux amd64/arm64 and
  darwin arm64. The other three have no upstream binary; Go there is served by
  `gopls` and by the dependence engine, which is a reduction in precise Go
  indexing on those platforms and is stated in the lock by omission.
- **`rust-analyzer`, `clangd`, `scip-clang`: no upstream digest.** These
  projects publish release assets with no checksum sidecar. The lock pins the
  SHA-256 observed at generation time, which from that point forward is what
  every fetch is verified against. `rust-analyzer`'s `version` is the upstream
  release tag, which is the store path component; the binary itself reports a
  `0.3.x-standalone` string, and both name the same software.
- **C/C++ coverage is split.** `scip-clang` gives precise C/C++ where upstream
  publishes it; `clangd` covers the platforms upstream builds it for. Neither
  is available on arm64 Windows, which the lock states by omission.
- **Three upstream assets are single files, not archives.** `rust-analyzer`'s
  unix builds are a bare gzip member, and `scip-clang` and `scip-java` are
  published unarchived. Extracting those needs a single-file payload shape in
  the runtime extractor; see the lane A3 report.

## Regenerating and verifying

```bash
go run ./internal/tools/toollock              # pin upstream, build and publish what we host, write the lock
go run ./internal/tools/toollock -check       # re-download every URL in the lock and confirm digests
go run ./internal/tools/toollock -tools joern # limit to one tool
go run ./internal/tools/toollock -no-upload   # produce the hosted payloads without publishing
```

`-check` is the release gate for "the lock describes what is actually
published". It makes no distinction between an upstream URL and one of ours:
every URL is fetched again and digested. A `tar.gz` or bare gzip payload is
hashed as it streams; a zip needs its central directory, which lives at the
end, so it streams past a scratch file that is deleted immediately. No payload
is ever held in memory, and a digest mismatch is never retried and never
executed.

The generator resumes: each finished platform is recorded, so an interrupted
run does not repeat work that already succeeded, and a hosted asset that is
already published at the right size is not uploaded again.

## Licenses

Every pinned payload's license is recorded in the lock entry and in
`THIRD_PARTY_LICENSES.md`. No pinned tool requires a paid service, a network
service at query time, or an AI API key.
