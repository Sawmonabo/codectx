# The managed analyzer toolchain: what is pinned and why

codectx owns every external analyzer it runs and every runtime those analyzers
need. codectx looks nothing up on `PATH`, installs nothing on the user's behalf
beyond the payloads its own lock pins, and executes nothing the shipped binary
did not pin — with one documented exception, the three host toolchains three of
the indexers shell out to (see [Host dependencies](#host-dependencies)). This document describes
the pinned set and the provenance behind it, the `codectx tools` commands that
manage it, and the per-platform matrix that proves it (Section 11.7). Store
layout, resolution order and offline refusals are implemented in
`internal/toolchain`, whose package comment is the detailed reference.

**The user installs no analyzer, no runtime and no language server.** Node comes
from a pinned nodejs.org distribution and the JDK from a pinned Eclipse Temurin
build; the four Node-hosted analyzers ship as prebuilt `node_modules` trees
produced at release time by `npm ci --omit=dev`, so no `npm install` ever runs on
a user's machine; `gopls` is cross-compiled at release time for all six targets.
Installing codectx installs nothing else.

**Three host programs are the exception, and they are the indexed language's own
toolchain**, because the indexer loads the project model through it exactly as a
build would — [the host dependencies](#host-dependencies) below is the one place
they are listed.

## The three artifacts

| Artifact | Produced by | Role |
|---|---|---|
| `internal/toolchain/tools.lock.json` | `internal/tools/toollock` | The embedded lock. One entry per tool with its version, license, upstream, runtime and a per-platform `{url, sha256, size, entry, entry_sha256, provenance}`. Data only. |
| `tools-v<n>` release assets | `internal/tools/toollock` | One deterministic `<tool>-<version>-<os>-<arch>.tar.gz` per hosted platform, for every tool-and-platform upstream publishes no binary for. |
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

The rule is applied per *platform*, not per tool: a tool whose upstream builds
some targets and not others is pinned upstream where a binary exists and hosted
where none does.

| Hosted on `tools-v1` (no upstream binary exists) | Pinned at the upstream URL |
|---|---|
| `gopls` — upstream ships no binaries; cross-built for all six | `node`, `jdk`, `joern`, `jdtls`, `clangd`, `scip-java`, `scip-clang`, `rust-analyzer` |
| `scip-typescript`, `scip-python`, `typescript-language-server`, `pyright` — npm packages, prebuilt so the user never runs npm | |
| `scip-go` — darwin amd64 and both Windows targets only; cross-built | `scip-go` — linux amd64/arm64 and darwin arm64 |

The hosted payloads are normalized deterministically: entries sorted by
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
| `scip-go` | 0.2.7 | indexer | — | go | all six | upstream with sidecar `.sha256` on linux amd64/arm64 and darwin arm64; cross-built and hosted on the other three |
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
- **`scip-go` is hybrid within one tool.** Upstream builds linux amd64/arm64
  and darwin arm64, and those three are pinned at the upstream assets. Upstream
  publishes nothing for darwin amd64 or either Windows target, so those three
  are cross-built from the pinned module at release time and hosted, exactly as
  `gopls` is. Precise Go indexing is therefore available on all six platforms.
  The module declares itself as `github.com/scip-code/scip-go` while the release
  assets still live under `github.com/sourcegraph/scip-go`; the project moved
  owner and both spellings name scip-go 0.2.7.
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
  published unarchived. The runtime writes such a payload as its single entry
  file, at the payload-relative path the lock already pins for it and with the
  executable bit set; the pinned entry digest then verifies it exactly as it
  verifies a member of an archive.

## The `codectx tools` commands

Nothing here is a prerequisite. Indexing resolves what a repository needs and
installs it on demand, so a user who never runs `codectx tools` gets the same
result. These commands exist for CI, for offline hosts, and for answering "what
is on this machine and is it still intact".

```bash
codectx tools status [--repo PATH] [--json]            # every lock entry and what the store holds
codectx tools prefetch [--all | --for-repo PATH | NAME...] [--json]
                                                       # install ahead of time
codectx tools verify [--json]                          # rehash every installed entry against the lock
codectx tools gc [--json]                              # remove versions the current lock does not name
```

`status` is the cheap report: it checks each entry's publication marker and the
presence of its executable, and never rehashes, so it stays usable for a store
holding the 1.8 GB engine. Each entry is reported in one of five states —
`installed`, `available` (pinned but not fetched yet), `unsupported_platform`
(the lock carries no payload here, which is honest absence and not a failure),
`override` (a `[tools.override.<name>]` replaces it) or `corrupt`.

`prefetch` requires one of `--all`, `--for-repo PATH` or explicit names, so a
bare invocation cannot start a multi-gigabyte download by accident. A name the
lock does not carry is rejected before anything is fetched. An entry with no
payload for this platform is skipped rather than failed. Each completed fetch
logs one record — tool, version, digest, bytes, elapsed — on stderr, and the
report of what is now installed goes to stdout.

`--for-repo PATH` installs what that repository's **root** selects, which on a Go
repository is four payloads rather than fourteen. The selection is read from the
mappings that already decide it — the SCIP indexers' trigger manifests, the
language servers' root markers, the dependence families' project markers, and
the path-to-language table for the source check below — plus each selected
entry's `runtime` from the lock, so no second copy of any of them exists
anywhere in the CLI. Everything is read at the repository root, by metadata or
by filename, through the confined root handle for the markers and as one listing
of the root directory for the sources: nothing is opened, nothing is started,
and nothing below the root is walked, so the work is bounded by the number of
markers plus the entries of one directory rather than by the size of the
repository. A root that selects nothing is an argument error rather than a
silent no-op.

The root is the whole of what the command can see, and that is the planner's
answer for two of the three providers but not the third:

- **SCIP and LSP are root-only too.** Both detect from manifests at the
  repository root, so what `--for-repo` selects for them is exactly what an
  index run resolves. A nested `svc/pom.xml` selects no Java payload here, and
  it selects none at index time either.
- **The dependence provider walks for sources.** Its C/C++ family declares no
  project marker at all — its unit is the repository itself, planned
  unconditionally — and its other families' units are found by walking the
  tree. `--for-repo` therefore selects the graph engine and its JDK on three
  signals: a project marker of one of the other families at the root
  (`go.mod`, `pom.xml`, `build.gradle`, `build.gradle.kts`, `tsconfig.json`,
  `jsconfig.json`, `package.json`, `pyproject.toml`, `setup.py`, `setup.cfg`,
  `Cargo.toml`), a C or C++ build declaration at the root (`CMakeLists.txt` or
  `compile_commands.json`, which also cover the out-of-source layout with
  sources under `src/`; a bare `Makefile` does not count, since it is common at
  roots of every language), or a source file of any of the families lying at
  the root itself. This over-selects in one direction only: a root carrying
  `CMakeLists.txt` or a project marker with no source of that family anywhere
  in the tree still prefetches the engine, because the planner gates on sources
  while this command reads only the root.

  One root shape is therefore still under-served and fetches the graph engine
  and its JDK at index time after a `--for-repo` prefetch reported success: a
  root that declares nothing at all while its sources live further down.
  Closing it would need a walk, which this command refuses so a prefetch's
  work stays bounded. When prefetching for an air-gapped runner from that
  shape, name the tools explicitly or use `--all`.

```console
$ codectx tools prefetch --for-repo .
NAME     VERSION      STATE      LANGUAGES
gopls    0.23.0       installed  go
jdk      21.0.12.1+1  installed
joern    4.0.627      installed  c, cpp, go, java, javascript, python, rust, tsx, typescript
scip-go  0.2.7        installed  go
```

`verify` is the expensive one: it rehashes each installed **entry executable**
against the digest the lock pins for it — the binary, script or jar the lock
names, and nothing else in the payload tree. A store a user edited or a disk
damaged is reported here rather than discovered inside an analyzer run, and a
corrupt entry **fails the command** with `CTX_TOOL_CORRUPT` — a check that
reports damage and exits 0 is a gate that passes silently in CI. A corrupt entry
is repaired by re-running `prefetch` for it, not by `gc`: `gc` removes versions
the lock no longer names, and a damaged tree at the pinned version is one the
lock does name.

What `verify` does **not** cover is the rest of the payload: a file added,
changed or removed anywhere but the pinned entry is not detected, because no
per-file digest of the tree is recorded at install time. That is why a managed
tool is never given the payload directory to write into — every profile points a
tool's own state at its run's work directory — and why an operator who suspects a
tampered tree reinstalls it with `prefetch` rather than trusting a green
`verify`.

The store is **one machine-wide directory shared by every workspace**:
`$XDG_DATA_HOME/codectx/tools`, or `~/.local/share/codectx/tools` when
`XDG_DATA_HOME` is unset or not absolute. That is the default, and it is the same
path the installer populates from a bundle, so a payload installed once is
installed for every repository on the host. The payloads are gigabytes and are
byte-identical for every checkout; a per-workspace store would fetch them again
for each one and leave a fresh clone with nothing installed.

`tools.cache_dir` in the user configuration overrides it. The path it names **is**
the store, used verbatim, not a parent that `tools` appends to. `--repo` selects
which workspace's resolved configuration is read, which matters only on a host
whose user configuration varies by repository. The path the reports print is the
one the resolver actually reads, so the two can never disagree.

Older builds put the store at `<data_dir>/tools`, which was per workspace. Those
directories are inert: nothing reads them any more, and each may be deleted.

None of the four commands creates anything it only reports on: `status`,
`verify` and `gc` leave a machine with no store exactly as they found it, and the
store directory appears when the first payload is installed.

Exit codes follow Section 18.2: every `CTX_TOOL_*` failure is exit 5 (a managed
tool that cannot be resolved is a provider that cannot run), except
`CTX_TOOL_OVERRIDE_INVALID`, which is exit 3 because it is a configuration error
the user fixes in their own file.

## Offline hosts and bundle archives

Two supported ways to run with no network at all, and one rule that governs
both: **codectx never fetches anything the embedded lock does not name**, so
"offline" is a refusal path, not a best-effort degradation.

**A bundle archive** (`codectx-bundle_<version>_<os>_<arch>.tar.gz`) carries the
binary with the tool store already populated for that one platform. The
installer unpacks it into the shared store the default already resolves to, so
only `[tools] offline = true` has to be set, and nothing ever dials out.
Each bundle is built natively for its own target, so one host cannot produce
another platform's bundle.

**A slim archive plus a prefetch** does the same in two steps on a host that
does have a network at install time:

```bash
codectx tools prefetch --all   # every lock entry for THIS platform
codectx tools verify           # rehash the store against the lock
# then, in the user configuration file:
#   [tools]
#   offline = true
```

`prefetch --all` installs every entry the lock carries **for this platform**;
there is no target flag, for the same reason there is no cross-built bundle.
`--for-repo PATH` narrows it to what that repository's root actually selects,
which is usually a much smaller store.

With `[tools] offline = true` every fetch becomes a typed refusal that opens no
socket. A tool already in the store still runs, so an offline host is fully
functional for every entry it holds and honestly unavailable for the rest —
`codectx tools status` names which is which, and `codectx doctor --offline`
reports the offline-policy checks. What that report says is what those checks
found; the flag itself asserts nothing about the installation.

`[tools] mirror` is the third option, for a host that has a network but not the
publishers': it relocates bytes and never changes which bytes are accepted,
because the digests stay the lock's. See
[Configuration](configuration.md#tools--the-managed-analyzer-toolchain).

## Host dependencies

Three host programs are not pinned and are not installed, because they are the
indexed language's own toolchain and the indexer loads the project model through
them:

| Program | Needed by | Language |
|---|---|---|
| `go` | `scip-go` | go |
| `cargo` | `rust-analyzer` | rust |
| `python3`/`pip3` | `scip-python` | python |

Nothing else needs anything on the host. Java in particular does not: `scip-java`
is driven from a `--scip-config` project description compiled with the managed
JDK's own `javac`, so neither Maven nor Gradle is required. Where one of the
three is absent, that language falls back to the structural and dependence
providers and is reported at the precision they give; it is never a failure of
another language. On the matrix runner the same absence **fails the leg**: a
language the runner cannot prove is the platform going uncovered, which is
exactly what the gate exists to catch.

## The per-platform matrix

`.github/workflows/tools-matrix.yml` is the gate behind "supported". On
`ubuntu-latest`, `macos-latest` and `windows-latest` it prefetches every payload
the lock carries for that platform, re-verifies the store, and then runs
`.github/tools-matrix --store <the store>`, which indexes a nine-language
fixture — Go, TypeScript, TSX, JavaScript, Python, Java, C, C++ and Rust, every
one of them carrying non-ASCII identifiers and literals — with the pinned
analyzers and parses each language family with the pinned graph engine.

Every indexer run issues **the product's own argument array**: the smoke builds
it with `scip.Argv`, the single source of every indexer invocation this product
makes, and lays the run out the way the provider does — the index written
outside the input directory, a private scratch root per run. A matrix that ran a
different argv would prove that some invocation works on the platform rather
than that the one the product issues does. The fixture and the assertion that
each index names the documents it was given are the smoke's own. C++ and TSX are exercised
explicitly because the research rounds covered only C and TypeScript. A language
no run covers fails the job, so a payload that silently stopped working cannot
leave a green matrix behind it.

The matrix does not run on every push: one leg downloads about 2.6 GB. It runs
when the lock, the toolchain runtime or the matrix itself changes, on demand,
and weekly — the weekly run is what catches an upstream asset that disappeared
from its publisher's release while its digest in the lock stayed valid.

The ordinary CI workflow enforces the other half of Section 11.7: `go list
-deps` over `./cmd/codectx` fails the build if any package outside
`internal/toolchain` imports `net/http`. The shipped binary's dependency graph
is what is checked, not a grep of the source.

## Regenerating and verifying

```bash
go run ./internal/tools/toollock              # pin upstream, build and publish what we host, write the lock
go run ./internal/tools/toollock -check       # re-download every URL in the lock and confirm digests
go run ./internal/tools/toollock -tools joern # limit to one tool
go run ./internal/tools/toollock -no-upload   # produce the hosted payloads without publishing
```

`-check` is the release gate, and it answers two questions rather than one.

First, **the lock describes what is actually published**: every URL is fetched
again and digested, with no distinction between an upstream URL and one of ours.
A `tar.gz` or bare gzip payload is hashed as it streams; a zip needs its central
directory, which lives at the end, so it streams past a scratch file that is
deleted immediately. No payload is ever held in memory, and a digest mismatch is
never retried and never executed.

Second, **the runtime can actually install what the lock pins**: every entry
carrying a payload for the platform the check runs on is then installed through
`internal/toolchain` itself — its fetcher, its digest verification, its confined
extractor and its entry hashing — into a scratch store that is removed
afterwards unless `-keep` is given. A digest check alone cannot see the
extractor, and a payload the extractor refuses is a tool the product cannot run
no matter how well its digest matches. One platform per runner is enough: the
digest half covers the other five and the extractor is platform-independent.

The lock also has to stay inside the runtime's extraction bounds. The generator
measures what each payload expands to and fails the run — rather than logging a
number — when a payload exceeds the file count or the expansion budget
`internal/toolchain` enforces, so that ceiling is met at release time instead of
on a user's machine.

The generator resumes: each finished platform is recorded, so an interrupted
run does not repeat work that already succeeded, and a hosted asset that is
already published at the right size is not uploaded again.

`-check` runs **daily** in `.github/workflows/lock-check.yml`, and on any change
to `internal/toolchain/tools.lock.json`. A published tag is not immutable: an
upstream project re-uploaded the assets of an already-published release three
hours after the lock had recorded their digests, and the lock — honest when it
was written — was discovered to be stale only by a user's index refusing to
fetch the analyzer mid-run. A gate that runs only when we change the lock cannot
see a change upstream makes, so this one runs on a clock. It aborts at the first
payload whose served bytes disagree, naming the entry and platform; re-pin that
one entry with `-tools <name>`.

The same drift is visible **locally, without a network call**, in
`codectx tools verify`: each row carries `lock:` — the digest this binary pins
for the entry executable — beside `disk:`, the digest the installed bytes hash
to, with the full pair in the `--json` envelope as `entry_sha256` and
`installed_sha256`. `disk:-` is an entry nothing on disk hashed: not installed,
or corrupt, and the row beside it says which. `tools status` never prints a
`disk:` digest, because it does not rehash.

## Licenses

Every pinned payload's license is recorded in the lock entry and in
`THIRD_PARTY_LICENSES.md`. No pinned tool requires a paid service, a network
service at query time, or an AI API key.
