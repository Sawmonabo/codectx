# Security

codectx reads a repository you already trust enough to check out and answers
questions about it locally. This page states the trust boundary in operator
terms and how to report a problem. The design-level detail — every control and
the mechanism that enforces it — is in
[docs/threat-model.md](docs/threat-model.md).

## Supported versions

codectx is pre-release. Security fixes land on `main` and in the next tagged
release; there is no back-port branch and no long-term support line.

## What the trust boundary actually is

**codectx is a local, single-user tool, not a hostile multi-tenant sandbox.**
Everything runs as the invoking user, against that user's own data directory
(`0700`, with every file codectx writes itself `0600`).

Two consequences follow, and both are deliberate:

- **A process running as you is inside the blast radius.** It can modify the
  SQLite database, the cursor and receipt signing key, and the managed tool
  store. Such a process is **outside** the cryptographic trust boundary of
  signed cursors and source receipts. Those signatures prove a token came from
  this workspace's key — not that the key was unreachable. If you need receipts
  to survive a compromised same-user process, run codectx as a separate user or
  inside a container.
- **`argv` is not a sandbox, and clearing proxy variables is not a network
  boundary.** codectx reduces the ways untrusted repository content can *cause*
  execution: no shell, an allowlisted child environment, a private
  materialization the analyzer sees instead of your worktree, and bounded
  stdin/stdout/runtime/temp bytes. It does not confine an analyzer that has
  started. Strong denial needs an OS-level control — a container, a namespace,
  a seccomp or Job Object policy — applied by the deployment around the whole
  process tree.

The adversary this model takes seriously is therefore **the repository itself**:
hostile filenames, symlink races, repository-scoped configuration that names a
program to run, oversized or malformed analyzer output, poisoned external index
files, and requests engineered to exhaust memory, disk or file descriptors.

## What is enforced

- **Path confinement is an OS property, not a string check.** Every repository
  and snapshot read goes through an `os.Root` handle.
- **Git runs with a fixed environment allowlist**, no hooks, no index write, and
  every configured clean/smudge filter neutralized for the run. A Git older than
  2.31.0 is refused rather than run without that neutralization.
- **Every external analyzer is lock-verified before it executes** — exact
  version, per-platform URL and SHA-256 in a lock the binary embeds — or is a
  digest-verified user override.
- **`internal/toolchain` is the only product-path importer of `net/http`**, CI
  enforces that, and `[tools] offline = true` refuses every fetch without
  opening a socket.
- **No telemetry, no remote metrics endpoint, no AI or cloud service.**
- **Ordinary logs carry typed fields only.** Source bodies, task text, secrets,
  inherited environment and raw child stdout/stderr are excluded by default.
  Remediation text is generated from the typed error code, never from an
  analyzer's message.

Run `codectx doctor --offline` to see which of these an installation currently
reports, including whether an OS-level restriction is actually active.

## Known limitations

These are accepted, not oversights, and are listed in full under "Honest
limitations" in [docs/threat-model.md](docs/threat-model.md). In short: an
analyzer that runs, runs; lock verification proves *which* payload executed, not
what it did afterwards. Windows graceful stop does not reach a child started
without a console, so child-side cleanup there is handled by the collector
rather than by the child. A passing network-disabled run proves that workflow,
not that every tool on the host is offline.

## Reporting a vulnerability

Report privately — do not open a public issue for something exploitable.

Use GitHub's private vulnerability reporting on
<https://github.com/Sawmonabo/codectx> (**Security → Report a vulnerability**).

Please include the codectx version and commit (`codectx version --json`), the
platform, the configuration that reproduces it, and the smallest repository
fixture that triggers it. Do not attach source, secrets or absolute paths you
would not want retained in an issue.

Expect an acknowledgement within a few working days. There is no bounty
programme. Reports that reduce to the trust boundary above — for example, a
same-user process forging a receipt — will be closed with a pointer to this
page rather than treated as vulnerabilities, because that boundary is a
documented design decision.
