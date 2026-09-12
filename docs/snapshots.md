# Snapshots and the content-addressed store

`internal/snapshot` captures the exact bytes of a workspace into an immutable
manifest plus a local content-addressed store (CAS), serves them back with
verified integrity, and materializes private copies for external analyzers.
`internal/vcs/git` is the only place Git is executed. This document is the
operator- and contributor-facing description of what a snapshot is and is not.

## What a snapshot is

A snapshot is **the bytes actually present in the worktree at capture**, not
`HEAD`. Every eligible regular file is read through the root-confined opener
(`workspace.Root.Open`) and retained in the CAS; the manifest row records the
SHA-256 of those bytes. Git is consulted for membership, status and provenance
only.

Consequences worth stating plainly:

- A file that is *clean* under Git can still differ from its blob. Attributes
  such as `text eol=crlf` make the checkout CRLF while the blob is LF; the
  snapshot holds the CRLF bytes and records the blob's object ID as provenance.
- An edited file is captured as edited (`modified`), a staged new file as
  `added`, an untracked non-ignored file as `untracked`, and a tracked file
  missing from the worktree as a `deleted` tombstone (no content, zero size,
  Git object ID kept).
- A directory without Git is a legitimate workspace. Nothing tracks its files,
  so every row is `untracked`, there is no `HEAD`, and no provenance.
- Files larger than `workspace.max_parse_file_bytes` or
  `max_search_file_bytes` are retained in full. Those settings are analysis
  admission limits; a capability report says a file was skipped for parsing or
  search, never that it was dropped from the snapshot.
- Symbolic links are excluded by default (`workspace.follow_symlinks`).
  Tracked symlinks are counted in the capture notes as skipped.

### Identity

```text
FileID       = H("file-v1", repository_id, path)
ManifestHash = fold over rows in bytewise path order of
               (path, status, size, content_hash, executable, language)
SnapshotID   = H("snapshot-v1", repository_id, head_object_id,
                 source_policy_hash, ManifestHash)
```

The manifest is folded entry by entry from on-disk staging with
`model.Hasher`; no repository-sized string is built. Git object IDs are
provenance and are excluded from the hash; timestamps are excluded. Language is
derived from the path by the single table in `internal/snapshot/language.go`
and is part of the row, so it is part of the hash.

### `capture_consistency`

`validated_capture` is the default and the only value the builder ever chooses
on its own: an exact immutable manifest plus detected-change validation. A
filesystem does not offer a simultaneous read of every file, so the builder
re-checks membership and every captured file's size, modification time and
executable bit after the walk, recaptures what changed, and repeats at most
twice. If the third validation still finds changes the capture fails with
`CTX_SNAPSHOT_UNSTABLE` and nothing is published.

`operator_frozen` is set only when the caller passes `Builder.OperatorFrozen`,
meaning the operator supplied a quiescent or OS-snapshotted source. It is never
inferred from a quiet validation pass.

## How membership is decided

For a Git workspace the builder runs, through the shared process runner:

| Purpose | Command |
|---|---|
| HEAD provenance | `git -c core.fsmonitor=false rev-parse --verify --quiet HEAD` |
| Sparse checkout | `git -c core.fsmonitor=false config --type=bool --default=false core.sparseCheckout` |
| Index (modes, object IDs) | `git -c core.fsmonitor=false ls-files -z -s` |
| Changes and untracked paths | `git -c core.fsmonitor=false status -z --porcelain=v2 --no-renames --untracked-files=all` (`=no` when `include_untracked = false`) |
| Repair source | `git -c core.fsmonitor=false cat-file blob <oid>` |

The child environment is exactly `GIT_CONFIG_NOSYSTEM=1`,
`GIT_OPTIONAL_LOCKS=0`, `GIT_TERMINAL_PROMPT=0`, `LC_ALL=C` plus, when set in
the parent, `HOME`, `XDG_CONFIG_HOME`, `USERPROFILE`, `SYSTEMROOT`, `TMPDIR`,
`TEMP`, `TMP`. Nothing else is inherited. The git executable is resolved from
`PATH` once (`git.Locate`) and recorded as an absolute path; every run uses that
path with a literal argument array. None of these commands runs hooks or writes
the index; `core.fsmonitor` is forced off because `git status` would otherwise
start a repository-configured executable. The operator's own (user-level) Git
configuration is honored, consistent with the trust model in
`docs/configuration.md`.

Output is NUL-delimited and parsed as it streams into the capture's private
staging database, which lives under `<data>/staging/` for exactly one capture.
Rename detection is off: a rename is a deletion plus an addition, because file
identity is path identity.

The filesystem walk (`workspace.Walk`) then applies the configured policy —
vendor and generated exclusions, symlink policy, the unconditional exclusion of
`.git` and of the data directory when it lies inside the workspace, the file
budget — with two hooks answered from staging:

- **Ignore**: a path Git neither tracks nor lists as untracked is ignored (or
  belongs to a submodule or nested repository). Directories are pruned when
  nothing beneath them is known, so an ignored `node_modules` is never listed.
- **Force include**: a tracked path wins over ignore rules and the vendor and
  generated toggles, and every ancestor directory of a tracked path is opened
  even when excluded. Nothing overrides the unconditional exclusions, the
  symlink policy or the file budget.

Untracked files under an excluded directory (for example `vendor/` with
`index_vendor = false`) are not captured.

### Capture notes

`Builder.Notes()` reports, for the most recent build:

- **Sparse checkout** (`core.sparseCheckout = true`): tracked paths absent
  from the worktree are still recorded as `deleted` — the snapshot describes
  the worktree — but the note tells a reader why.
- **Submodules**: a tracked `.gitmodules` and each gitlink (`160000`) path.
  Submodule contents are another repository and are neither captured nor
  recursed into; nothing is fetched.
- **Git LFS pointers**: files whose captured bytes begin with
  `version https://git-lfs.github.com/spec/v1`. The pointer is what the
  worktree holds, so the pointer is what is retained; the large object is not
  fetched.
- **Skipped symlinks** and the number of validation **retries**.

Path lists in the notes are capped at 32 entries; the counts beside them are
complete.

## The content-addressed store

```text
<data>/cas/tmp/            private files being written
<data>/cas/<hh>/<sha256>   published immutable blobs (0400)
```

`CAS.Put` streams a file through `source.BuildIndex`, which computes the
whole-file SHA-256, one digest per 64-KiB block and sparse line checkpoints in
the same pass, while the bytes are written to a private temporary file. The
file is fsynced, made read-only and published with `os.Link` to a name derived
only from the validated digest; an object already present is kept (its size is
checked), never replaced. Where hard links are unavailable a rename is used
only while the target is absent. The bucket directory is fsynced on Unix; on
Windows there is no directory fsync and NTFS journaling is what makes the new
name durable — this is the documented platform difference.

The block digests and checkpoints are persisted with `storage.PutBlob` before
the manifest row that names the blob is written, and `PutSnapshot` refuses a
manifest row whose blob is not retained. Publication of the snapshot therefore
cannot precede retention of its bytes. Identical content at several paths is
one object. Blobs are raw files: no compression, no mmap, no packing.

## Reading

`snapshot.View` implements `model.SnapshotView`:

- `Open` streams the blob one block at a time, verifying every block digest as
  it passes and the whole-file digest and size at end of file. Memory is one
  block.
- `ReadRange` reads and verifies exactly the blocks covering the interval and
  claims nothing about blocks it did not read. An interval is bounded by
  `model.MaxRawChunkBytes` (1 MiB).
- `Read` is `ReadRange` plus one-based line and zero-based byte-column
  positions for both ends, derived from the nearest stored checkpoint at or
  before the interval through `source.NewCursorAt`. The window from that
  checkpoint to the interval's end is what is read and verified; nothing before
  the checkpoint is touched. A boundary inside a UTF-8 sequence is rejected.

Reads never fall back to the live checkout or to Git. A missing, truncated,
oversized or corrupt object is `CTX_SOURCE_INTEGRITY`.

Known limitation: checkpoints are recorded at line starts only, so the position
window for a byte inside a single line longer than the read ceiling exceeds
that ceiling and `Read` returns `CTX_RESOURCE_LIMIT` (bytes are still available
through `ReadRange`). A checkpoint whose `line_start_byte` precedes its
`byte_offset`, which the schema already permits, would bound the window to one
block; that is a change to the shared index builder.

### Repair

`View.Repair(ctx, fileID, RepairSource{Git, Root})` is the one explicit path
that consults Git for content. It reconstructs a *missing* object from the
recorded Git object ID and publishes it only if the complete SHA-256 equals the
manifest's content hash; a different digest, an object larger than the recorded
size, or a missing object leaves the store untouched and returns
`CTX_SOURCE_INTEGRITY`. A present object is verified, not replaced. Repair
never changes a snapshot's identity. Because a clean file's blob may legally
differ from its captured bytes (attributes), repair of such a file is refused,
correctly.

## Materialization

`Materialize(ctx, view, selection, options)` copies the selected nondeleted
files into a fresh private directory `<data>/materialize/mat-<id>/`, preserving
the root-relative layout and the executable bit (`0700` versus `0600`). Every
byte passes through the view's verified reader; files are copies, never hard
links to the CAS or the repository, so an analyzer that writes cannot alter
retained source. Writes go through an `os.Root` over the new directory, so a
manifest path can neither escape it nor follow a link out of it. Total bytes
are bounded by `MaxBytes` (`CTX_RESOURCE_LIMIT` when exceeded). The tree is
removed on any failure or cancellation, and `Close` (idempotent) removes it on
success. Each materialization holds an OS lock on `mat-<id>.lock` while alive;
`Sweep` removes materializations whose lock is free — their owner is gone — and
leaves live ones alone.

## Locking and recovery

`snapshot.LockWorkspace(ctx, dataDir, wait)` is the cross-process indexing/GC
coordination lock: `<data>/workspace.lock` held with `flock` (Unix) or
`LockFileEx` (Windows). A capture, the indexing that follows it, publication,
retention collection and startup recovery all take this one lock, so a
collector cannot see a capture in progress and two processes cannot build
competing generations. A second caller gets a retryable `CTX_WORKSPACE_BUSY`
after `wait` (or at once when `wait <= 0`). The lock is per open file: the
outermost owner acquires it once and passes it to `Builder.Lock`; a builder
given no lock acquires one for the duration of the capture.

`snapshot.Sweep(dataDir)`, run by the lock holder at startup, removes staging
databases left by a crashed capture and abandoned materializations.

## Errors

| Situation | Code |
|---|---|
| Worktree kept changing after two recaptures | `CTX_SNAPSHOT_UNSTABLE` (retryable) |
| Missing, truncated, oversized or corrupt blob; repair digest mismatch | `CTX_SOURCE_INTEGRITY` |
| Lock held by another process | `CTX_WORKSPACE_BUSY` (retryable) |
| File budget, listing bound, read ceiling, materialization bound | `CTX_RESOURCE_LIMIT` |
| Data directory disk full | `CTX_DISK_FULL` |
| Git repository without a git executable, git failure | `CTX_PROVIDER_UNAVAILABLE` |
| Git output this build cannot parse | `CTX_PROVIDER_OUTPUT_INVALID` |
| Path or range shape errors | `CTX_ARGUMENT_INVALID` |

Log lines carry identifiers and counts only: never a source body, and never a
repository path.
