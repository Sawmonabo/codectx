# ADR-0018: Source is read by node or byte range from a pinned generation, with or without a context session

## Status

Accepted, 2026-10-09, by the owner's ruling. Not yet implemented. The survey behind it is
[research 27](../research/27-read-source-outside-a-session.md).

## Context

- **A read needs a session.** `codectx_read_source` requires a session and an actor (`internal/model/coverage.go:68-86`)
  and answers with a receipt (`coverage.go:152`). The read checks the session first (`internal/coverage/read.go:58`),
  refuses a file outside the session's pinned scope, and persists the issued chunk before it signs the receipt
  (`read.go:174-195`).
- **Worked example.** An agent asks "who calls `Authorize`?" and gets rows with a file and a range. To read the 30
  lines of `Authorize` it must open a context session (a task text, seeds, budgets), find the file, read a chunk and
  leave an unconfirmed receipt behind, or leave the index and read the working tree, which may no longer match the
  generation that answered.
- **Only the receipt needs the session.** Coverage exists to answer which bytes an actor was served and confirmed
  (`docs/context-sessions.md:3-5`). Pinning does not need a session: every query answer pins one generation through a
  query lease (`internal/app/services.go:723-747`), and a continuation cursor carries a lease that expires with it
  (`internal/graph/cursor.go:785-798`). The bounds on a read are the chunk size (1 MiB at most,
  `internal/model/validate.go:39`) and the source wire bound (7 MiB, `internal/config/config.go:582`, checked in
  `internal/mcpserver/session.go:146`); neither comes from the session.
- **What exists already.** The snapshot view reads verified bytes from the content store with positions and takes no
  session (`internal/snapshot/view.go:171-232`); reads never fall back to the working tree (`docs/snapshots.md:270`).
  The chunk planner owns every boundary rule: no start inside a UTF-8 sequence, whole lines when they fit, an
  over-long line split with `partial_line`, invalid UTF-8 as base64 (`internal/source/chunk.go:20-34`). A node carries
  its file, content hash and range (`internal/model/facts.go:122-124`).
- **The requirement.** Complete answers with nothing to tune; no clipping and no fixed cap; wire-security bounds and
  lossless paging stay; one tool returns source.

## Decision

### 1. `codectx_read_source` reads outside a session, and earns nothing there

- With `session_id` the call is the session read, unchanged: actor, scope, issued chunk, receipt, coverage.
- Without `session_id` it takes exactly one selector: `node_id`, the node's range; or `file_id` (or `path`) with
  `offset` and `max_bytes`, a byte range, from the start of the file when no range is given. It also takes an
  optional `generation_id` (the active generation when absent) and `cursor`.
- A read outside a session writes no issued-chunk row, signs no receipt and changes no coverage.
- The CLI gains `codectx source <node-id | file-id | path>` with `--offset`, `--max-bytes`, `--generation` and
  `--json`; it loops the chunks itself and writes the whole span verbatim, holding one chunk in memory.

**Alternatives.** Session reads only: an agent opens a session for 30 lines or leaves the index for a working tree
that can disagree with the answer, unlike every comparable tool [S1][S2]. A body flag on the symbol and graph tools:
every graph answer becomes a source answer under the source bound instead of the metadata bound, and a page of callers
with bodies is the oversized answer agent-tool guidance warns against [S3]. Snapshot files as protocol resources: the
host decides which resources to load, so a model cannot reliably pull one, and a resource read takes no range [S4]; it
can be added later, beside this, not instead of it.

### 2. One answer shape, the session's fields in one optional block

The answer carries `binding`, `file_id`, `node_id` (for a node read), `content_hash`, `byte_range`, `line_range`,
`encoding`, `content`, `partial_line`, `next_offset` and `next_cursor`, and a `session` block holding `receipt` and
`coverage`, present only for a session read. Their absence is structural, never an empty string.

### 3. The generation is pinned, and a node resolves in it

- The read is pinned to `generation_id`, or to the active generation, by a query lease for the call, as every query
  is.
- A node id is resolved in that generation. Under [ADR-0014](ADR-0014-stable-identity-incremental-linking.md) one node
  id can live in several generations at different ranges, so the read uses the pinned generation's range and file
  row, which agree by construction. A node that moved is read at the asked generation's range.
- A node absent from the generation (deleted, or a working-tree overlay row, which is never a canonical fact) is
  refused as "not in this generation" (`CTX_ARGUMENT_INVALID`).
- The node's content hash against the snapshot's file row is an integrity check (`CTX_SOURCE_INTEGRITY` on mismatch),
  not a normal refusal.
- Passing the `generation_id` from a graph answer's binding makes the bytes match that answer's ranges exactly
  ([ADR-0017](ADR-0017-extraction-and-linking-coverage.md) decision 1).

### 4. Size: whole when it fits, paged losslessly when it does not

- A node read returns the whole node in one call whenever it fits one chunk, which is nearly every symbol. It is never
  clipped and never refused for size.
- A larger span pages: `next_offset` and a signed `next_cursor` carrying a cursor lease, the convention every other
  continuation uses, so the generation cannot be collected between pages. A node's continuation ends at the node's
  end, not the file's.
- The chunk bound and the source wire bound are wire-security bounds and apply to both modes.
- No client-specific result-size key is set on the tool: an exact-symbol read is small, and a whole-file read that a
  client stores as a file instead of inlining it is acceptable.

**Alternative.** A line or character cap with a truncation notice, as several code-graph servers do [S5][S6], or an
empty answer over a size limit [S7]: both are the shortcuts the product's no-limits rule rules out.

### 5. Every byte as stored

A chunk that is not valid UTF-8 is returned as base64 with `encoding: "base64"` and byte ranges, never with
replacement characters, through the same chunk planner as a session read.

### 6. Reads go by file id into the content store, never through a path

A `path` argument is first put in the spelling the capture stores — slash-separated, lexically clean and
relative to the repository root, so `./pkg/a.go` and `pkg/../pkg/a.go` both name `pkg/a.go` — and a path that is
absolute or climbs above the root is refused as `CTX_ARGUMENT_INVALID`. The clean path is only hashed into a file
id and looked up in the generation's manifest; a path that is not a member is "not in this generation", and
nothing is opened on it. Only files the index captured can be read, never
anything outside the tree, an ignored file or the data directory. Capture already goes through an `os.Root` handle
that refuses escapes by `..` and symbolic links (`docs/threat-model.md:35`), and symbolic links are excluded by
default (`docs/snapshots.md:32`). The CLI reads the same content store, never the working tree. A file server that
checked containment by string prefix and validated a link instead of its target is the failure this avoids [S8].

### 7. The other tools stay metadata only

`symbol_info`, `find_symbol`, `callers` and `callees` return no source. `codectx_read_source` is still the only tool
that returns source; the tool count stays 23 (`internal/mcpserver/registry.go:94`). Text that says source needs a
session is updated where it says so; the invariant that one tool returns source stays.

### 8. Proof

- A node read returns exactly the node's bytes and the binding of the generation asked for.
- A node that moved between two generations is read at each generation's own range (mutation: resolve the node in
  the active generation instead of the asked one).
- A node absent from the asked generation is "not in this generation".
- A range over one chunk pages to the node's end with no gap and no overlap, and survives a retention pass between
  pages (mutation: clip at one chunk).
- A file that is not valid UTF-8 comes back base64 with byte ranges.
- A path not in the snapshot, `../` and a symbolically linked path included, is "not in this generation" (mutation:
  read the working tree).
- A read outside a session creates no issued chunk and changes no coverage; a session read is unchanged.

## Consequences

- An agent reads exactly the symbol a graph answer named, from the same generation, in one call.
- Coverage still means what it meant: only bytes served in a session and confirmed by its actor count.
- The read tool has two modes, and its description says that a read outside a session earns no coverage.
- The CLI gains one command.

## Sources

- [S1] *GitHub MCP Server*, https://raw.githubusercontent.com/github/github-mcp-server/main/README.md —
  `get_file_contents` pinned by `ref` or `sha`.
- [S2] *Sourcegraph MCP Server*, https://7.6.sourcegraph.com/api/mcp.md — `read_file` with line ranges and a
  revision.
- [S3] *Writing effective tools for AI agents*, https://www.anthropic.com/engineering/writing-tools-for-agents —
  pagination, range selection and truncation; "only high signal information".
- [S4] *MCP specification 2025-06-18, Resources*, https://modelcontextprotocol.io/specification/2025-06-18/server/resources
  — resources are application-driven.
- [S5] *codebase-memory-mcp mcp.c*, https://github.com/DeusData/codebase-memory-mcp/blob/main/src/mcp/mcp.c — a
  500-line clip, and indexed line numbers applied to the live file.
- [S6] *Sourcegraph MCP Server*, as [S2] — files over 128 KB return the first 200 lines.
- [S7] *Serena tools_base.py*, https://raw.githubusercontent.com/oraios/serena/main/src/serena/tools/tools_base.py —
  over the limit, no content is returned.
- [S8] *EscapeRoute: CVE-2025-53109 and CVE-2025-53110*, https://cymulate.com/blog/cve-2025-53109-53110-escaperoute-anthropic/
  — prefix containment and symbolic-link checks; and *Traversal-resistant file APIs*, https://go.dev/blog/osroot.
- [research 27](../research/27-read-source-outside-a-session.md) and
  [raw/read-source](../research/raw/read-source/findings.md) — the survey, the options and the full source list.
