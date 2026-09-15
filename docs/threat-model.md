# Threat model

`codectx` reads a repository you already trust enough to check out, and answers
questions about it offline. This document states what it defends against, how,
and — just as importantly — what it does **not** defend against, so an operator
can tell a control from a convenience.

## What this is, and what it is not

codectx is a **local, single-user tool**, not a hostile multi-tenant sandbox.
Everything runs as the invoking user, against that user's data directory.

Two consequences follow, and both are deliberate:

- **A same-user process is inside the blast radius.** Another process running as
  you can modify the database, the cursor/receipt signing key and the tool store.
  Such a process is *outside* the cryptographic trust boundary of signed cursors
  and source receipts: those signatures prove that a token came from this
  workspace's key, not that the key was never reachable.
- **`argv` alone is not a sandbox, and clearing proxy variables is not a
  security boundary.** The controls below reduce the ways untrusted repository
  content can *cause* execution. They do not confine an analyzer that executes.
  Strong denial needs an OS-level control — a container, a namespace, a seccomp
  or job policy — applied around the whole process by the deployment.

The adversary this model takes seriously is therefore **the repository itself**:
hostile filenames, symlink races, repository-scoped configuration that names a
program to run, oversized or malformed analyzer output, poisoned external index
files, and requests engineered to exhaust memory, disk or file descriptors.

## Boundaries and the controls that hold them

| Boundary | What is actually enforced |
|---|---|
| **Filesystem** | Every repository and snapshot read goes through an `os.Root` handle (`internal/workspace`, `internal/snapshot/materialize.go`), so path confinement is an OS-enforced property of the file descriptor, not a string comparison that a symlink can race. Absolute and unmapped paths, `..` components, NUL bytes and unsupported file types are rejected before an open is attempted. Distinct valid paths that differ only by case or Unicode form are never aliased together. |
| **Git** | One argv-based runner owns every Git invocation (`internal/vcs/git`), with an absolute executable resolved once, NUL-delimited output parsing and a **fixed environment allowlist** — the child never inherits your environment. Hooks are not run, the index is not written (`GIT_OPTIONAL_LOCKS=0`), the fsmonitor hook is disabled, and every configured clean/smudge filter driver is neutralized for the run through paired `GIT_CONFIG_KEY_<i>`/`GIT_CONFIG_VALUE_<i>` variables. That mechanism needs Git 2.31.0 or newer, so an older Git is **refused** with a typed `CTX_PROVIDER_UNAVAILABLE` before any capture, rather than silently executing repository-configured filter commands. |
| **Analyzer execution** | An external analyzer runs only from a **lock-verified managed payload** or a **digest-verified user override**. The argv is built by codectx; there is no shell, so no interpolation. The analyzer sees a **private materialization** — copied files under a user-private directory, never hard links to the content store or to your worktree — so an analyzer that writes cannot alter retained source. Environment is allowlisted; stdin, stdout, stderr, temporary bytes and wall-clock runtime are all bounded. |
| **Project code execution** | Compilers, indexers and language servers *load project configuration, plugins and build macros by design*. codectx does not pretend otherwise: it runs them only against the private materialization above, and `doctor` discloses which of them are enabled and whether an OS-level restriction is actually active. This is disclosure plus isolation, not prevention. |
| **Process trees** | Children are started in their own process group on Unix and assigned to a Job Object on Windows, and stopped gracefully before a bounded forced kill. On Windows the child is created suspended and assigned to the job *before* it is resumed, so it cannot spawn an escaping descendant in the window between launch and assignment. See the limitation below. |
| **Native parser** | Grammar-driven parsing runs in a worker **subprocess of this same binary** with bounded input and IPC framing; every result is validated before it is stored. A native crash ends a worker, not the CLI or the MCP server. |
| **Network** | `internal/toolchain` is the **only product-path importer of `net/http`**, and it fetches nothing but payloads the embedded lock names, over HTTPS, checked against the lock's digest before anything is installed. Redirects are bounded and confined to the lock's host plus a small, explicitly recorded delegation set; any other host is refused. `tools.offline` disables the path entirely, as does a bundled distribution. (`internal/tools/toollock` also imports `net/http`; it is a release-time `package main` generator, not part of the shipped binary.) |
| **Provider data** | Imported indexes and protocol responses are validated *before* allocation: declared lengths, nesting depth, record fields, canonical identifier shape, source hashes, range encoding, capability claims and visible endpoints. A length field is never trusted as an allocation size. |
| **API** | Every CLI and MCP input is typed and bounded. Sessions check the actor, cursors and source receipts are signed and bounded by a TTL, responses are schema-validated, and cancellation is honored. No generic tool returns source bodies. |
| **Private storage** | The data directory and every tree codectx creates under it is `0700`, and every file codectx writes itself is `0600`: the cursor signing key, pagination spools, materialized analyzer input, and the tool store with its staging area. The database lives inside that private tree. No writable hard links are published into it. Retention is explicit and quota-bounded rather than unbounded growth. |

## Logging and disclosure

Ordinary logs carry typed fields only — component, repository, snapshot,
generation, provider, unit, run, operation, duration, status and error code.
**Source bodies, task text, secrets, inherited environment and raw child
stdout/stderr are excluded by default**, and raw analyzer output is never passed
through guess-based string redaction into an ordinary log. Filenames and other
repository-controlled strings are sanitized for control characters before human
display; JSON escaping is not terminal sanitization.

Diagnostic remediation is generated **from the typed error code**, never from an
analyzer's message text. Raw output is retained only in an explicitly requested
private debug artifact, with byte and lifetime caps.

There is no telemetry and no remote metrics endpoint.

## Honest limitations

These are known and accepted, not oversights.

- **Windows graceful stop does not reach a non-console child.** The graceful
  phase sends `CTRL_BREAK_EVENT` to the child's process group
  (`internal/process/process_windows.go:126-129`). A process started without a
  console never receives it, so it gets no chance to remove its own temporary
  files; the forced phase then terminates the Job Object, which does reach every
  descendant at once. Termination is therefore reliable on Windows, but
  *child-side cleanup is not*. The collector described in
  [operations](operations.md) is what reclaims what such a child leaves behind.
- **Process-tree memory sampling is Linux-only.** This is a measurement gap, not
  a control gap; see the platform table in [operations](operations.md). An
  unmeasurable figure is reported as unavailable, never as zero.
- **An analyzer that runs, runs.** Lock verification proves *which* payload
  executed. It says nothing about what that payload does once started.
- **Passing a network-disabled test proves that workflow.** It is not a
  universal claim that every third-party tool installed on the host is offline.
- **The receipt boundary is cryptographic, not custodial.** See the first
  section: same-user access to the signing key defeats it by design.
