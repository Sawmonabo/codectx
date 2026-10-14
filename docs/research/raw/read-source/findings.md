# Reading source outside a context session

Research of 2026-10-09 behind [ADR-0018](../../../adr/ADR-0018-read-source-outside-a-session.md). Code read in the
repository at commit e9c8799 (the code under `internal/` is unchanged on the branch these documents land on); paths are
repository paths.

## 1. The question, in plain words

An agent asks codectx "who calls `Authorize`?" and gets back rows with a file and a line range.
To look at the 30 lines of `Authorize` itself, it has two choices:

- open a whole context session (`codectx_context_plan` with a task text, seeds and budgets), find the
  file id, read a chunk, then acknowledge the receipt or leave it dangling; or
- leave codectx and read the working tree with its own file tool, which may no longer match the
  generation the answer came from.

The question: should `codectx_read_source` (and the CLI) also answer "give me the bytes of node N" or
"bytes 1200-2400 of file F" from the indexed snapshot, with no session, no receipt and no coverage
credit? `symbol_info` would stay metadata only.

Example of the proposed call:

```json
{"tool": "codectx_read_source", "arguments": {"node_id": "<64 hex>"}}
```

answers the exact bytes of that node's range from the generation the node came from, with
`binding`, `content_hash`, `byte_range`, `line_range`, `encoding`, and no receipt.

## 2. Why reads are session-bound

Four reasons, from the code. Only the first one actually needs a session. (Manifest reuse, where a
repeated `plan` reuses an existing manifest, `docs/context-sessions.md:86-88`, is a planning concern a
read never takes part in.)

1. **Receipts and coverage.** The session surface exists to answer "which bytes did this actor
   actually read?" (`docs/context-sessions.md:3-5`). `coverage.Service.Read` checks the session and
   actor first (`internal/coverage/read.go:58`), refuses any file outside the session's pinned scope
   (`read.go:224-238`, `CTX_SCOPE_INCOMPLETE`), persists the issued chunk before it signs a receipt
   (`read.go:174-195`), and only an echoed receipt grants coverage (`docs/context-sessions.md:128-138`).
   The request type itself requires `session_id` and `actor_id` (`internal/model/coverage.go:68-86`),
   and the response requires a receipt (`coverage.go:152`).
2. **Pinning.** Bytes come from the snapshot the session pinned, never from the working tree
   (`docs/context-sessions.md:118-119`; `read.go:102` opens `rec.Binding.SnapshotID`). A session keeps
   its generation alive against retention (`internal/storage/sqlite/retention.go:97-99`).
   But pinning does not need a session: every explore answer already pins one generation per call
   through a query lease (`internal/app/services.go:723-747`, `withEngine`), and continuation cursors
   carry a cursor lease that expires with the token (`internal/graph/cursor.go:785-798`,
   `storage.query_cursor_ttl` default 15 minutes, `internal/config/config.go:599`).
3. **Budgets.** The session's budgets bound a plan, not a read (`docs/context-sessions.md:37-46`).
   A read is bounded by the chunk size (`coverage.max_chunk_bytes`, default 1 MiB,
   `internal/config/config.go:652`; ceiling `model.MaxRawChunkBytes` = 1 MiB,
   `internal/model/validate.go:39`) and by the wire bound `resources.max_source_response_bytes`
   (7 MiB, `config.go:582`), checked in the handler (`internal/mcpserver/session.go:146-159`). Neither
   bound comes from the session.
4. **"One tool returns source" as a design invariant.** It is stated in many places:
   `internal/mcpserver/registry.go:72-73`, `internal/mcpserver/server.go:23-27` (server instructions),
   `internal/mcpserver/limits.go:38-45`, `internal/mcpserver/explore.go:109-110`,
   `internal/mcpserver/session.go:68-72` and `:115-117`, `internal/model/query.go:319-323`,
   `internal/app/services.go:503-504`, `docs/threat-model.md:43` ("No generic tool returns source
   bodies"). The point of the invariant is that every other answer stays small, metadata only and
   bounded by `max_tool_request_bytes`; it does not say source must need a session. Proposal (b)
   keeps the invariant (still one tool), it only drops the session requirement for a read that claims
   no credit.

What a session-free read can reuse as is:

- `snapshot.View.Read` takes no session: it reads verified bytes from the content store with line and
  column positions (`internal/snapshot/view.go:171-232`). Reads never fall back to the live checkout
  or Git (`docs/snapshots.md:270-271`).
- `source.PlanChunk` owns every boundary rule: no start inside a UTF-8 sequence, prefer whole lines,
  split an over-long line and say `partial_line`, carry invalid UTF-8 as base64, valid empty end of
  file (`internal/source/chunk.go:20-34`, `:75-131`).
- A node carries `FileID`, `ContentHash` and `Range` (`internal/model/facts.go:122-124`); explore
  requests already take an optional `generation_id` (`internal/model/query.go:422-423`).

## 3. What the field does

| Tool | How source is read | Size handling | Consistency | Path safety |
|---|---|---|---|---|
| MCP filesystem server `read_text_file` | whole file, or `head`/`tail` N lines; always treated as UTF-8 | no paging beyond head/tail | live disk | allowed directories or client roots; had two path-escape CVEs (prefix match, symlink) fixed in 2025.7.1 |
| GitHub MCP `get_file_contents` | `owner`, `repo`, `path`, optional `ref` or `sha` (sha wins) | REST contents API: full features up to 1 MB, raw only 1-100 MB, nothing over 100 MB | pinned by commit sha when given | server-side repo permissions |
| Sourcegraph MCP `read_file` | `repo`, `path`, optional `startLine`, `endLine`, `revision` | files over 128 KB return the first 200 lines with a truncation notice; use line ranges | pinned by `revision` | repo permissions |
| Serena `find_symbol(include_body=true)` / `read_file(start_line, end_line)` | body on the symbol answer; file or line chunk | `max_answer_chars`: over the limit, "no content will be returned" | language server over the project | "within the project directory" (docstring) |
| codebase-memory-mcp `get_code_snippet` | by qualified name; indexed start/end lines, then reads the file from disk | 500-line cap, sets `source_clipped`; `start_line`/`max_lines` to page | indexed line numbers applied to the live file (can drift after edits) | realpath containment check on every read |
| trace-mcp `get_symbol` (third-party listing only) | by symbol id or FQN, "returns only the symbol, not the whole file" | not stated | not stated | not stated |
| Claude Code `Read` | path, `offset`, `limit` lines | a whole-file read over the token limit returns a first page with a `PARTIAL view` notice | live disk | working-directory permissions |
| Aider repo map | signatures and key lines only; full files only once added to the chat | `--map-tokens` default 1k | live disk | n/a |
| LSP `textDocument/definition` | returns a `Location`/`LocationLink` (`targetRange` = full range), no text; the client reads the bytes | n/a | the client owns open documents; the server must not read them by URI | n/a |

What this says:

- **Exact span reads are the norm.** Every code-graph server above lets an agent read one symbol or
  one range without any session or ceremony (Serena, codebase-memory, trace-mcp, Sourcegraph, GitHub).
  Anthropic's tool guidance points the same way: use "pagination, range selection, filtering, and/or
  truncation" and return "only high signal information".
- **Pinning by revision is how the careful ones stay consistent.** GitHub (`sha`) and Sourcegraph
  (`revision`) read a fixed version. codebase-memory applies indexed line numbers to the live file, so
  after an edit the "symbol" can be the wrong lines. codectx's content store is already the pinned
  version, so it can do better than all of them: the node's range and the bytes come from the same
  content hash.
- **Size handling is where they go wrong.** Clipping (codebase-memory's 500 lines, Sourcegraph's first
  200 lines) or returning nothing (Serena) are the shortcuts the product's no-limits rule forbids. Paging with an
  explicit continuation is right (Claude Code `Read` itself pages with a notice).
- **Path checks are where they get hurt.** The filesystem server's two 2025 CVEs were a string-prefix
  containment check and a symlink check that validated the link instead of its target.

Client limits that matter for codectx: Claude Code warns at 10,000 tokens of MCP output, caps at
25,000 tokens by default, and saves any text result over 50,000 characters to a file; a server can
raise one tool's threshold with `_meta["anthropic/maxResultSizeChars"]`, up to 500,000 characters.
A default 1 MiB chunk is far above all of these, so a whole-file read reaches the client as a saved
file, not inline. An exact symbol (usually a few KB) does not.

## 4. Options

| | Option | For | Against |
|---|---|---|---|
| (a) | Keep session-only | No change; one read path; every byte served is accounted. | An agent that wants 30 lines must plan a session or leave codectx for the live tree, which can disagree with the generation that answered. Unlike every peer tool. Receipts it never confirms pile up as unconfirmed chunks. |
| (b) | **Proposed:** `codectx_read_source` also reads an exact node or byte range from a pinned generation outside a session; no receipt, no credit; CLI the same | Matches field practice; exact bytes from the same content hash as the fact; reuses `View.Read` and `PlanChunk`; still one source tool; tool count stays 23 (`registry.go:91-94`). | Two modes in one tool: the answer must make clear a session-free read earns nothing. Needs a continuation for spans over one chunk. |
| (c) | `include_body`/`source` flag on `symbol_info`/`callers`/`callees` | One call instead of two (Serena's precedent). | Breaks the one-source-tool invariant and `docs/threat-model.md:43`; every graph answer becomes a source answer with a 7 MiB bound instead of the metadata bound (`limits.go:38-45`), and a callers page of 50 rows each with a body is exactly the oversized answer the tool guidance warns about. The plan already rules `symbol_info` metadata only. |
| (d) | Expose snapshot files as MCP resources (`resources/read` on a template like `codectx://generation/{id}/file/{file_id}`), tools return `resource_link`s | Standard MCP shape; text or base64 `blob` built in. | Resources are "application-driven" (the host decides what to load), so a model cannot reliably pull one; no range parameter in `resources/read`; the server registers no resources today (`registry.go:22-23`). A later addition, not a replacement. |

## 5. Recommendation: (b), with these rules

Choose (b). It is what every comparable tool does, the parts already exist, and codectx can do it
more exactly than the field because the bytes and the fact share one content hash.

**Request.** `codectx_read_source` keeps its session fields optional. Without `session_id` it takes
exactly one selector:

- `node_id`: read that node's range;
- `file_id` (or `path`, turned into a file id with `model.NewFileID`) plus `offset` and `max_bytes`:
  read a byte range; no range means from the start;

plus optional `generation_id` (default: the active generation, as for every explore request) and
`cursor` for a continuation. With `session_id` the call behaves exactly as it does now.

**Answer.** Keep one output schema, but move the session-only fields into one optional block, so
their absence is structural rather than an empty string: `binding`, `file_id`, `node_id` (when
asked by node), `content_hash`, `byte_range`, `line_range`, `encoding`, `content`, `partial_line`,
`next_offset`, `next_cursor`, and `session: {receipt, coverage}` present only for a session read.
A session-free read never writes an issued-chunk row and never touches coverage.

**Which generation is read.** The one named by `generation_id`, else the active one, pinned for the
call with a query lease exactly as `withEngine` does (`services.go:723-747`). A node id is resolved
**in the pinned generation**: under name-path identity with the range as an attribute
([ADR-0014](../../../adr/ADR-0014-stable-identity-incremental-linking.md)) the same node id can live in several generations at different ranges, so the read uses
that generation's range and file row, which match by construction.

- node present in the generation: read at that generation's range, even if the node has moved since;
- node absent (deleted, or an LSP overlay row, which is never persisted as a canonical fact,
  `internal/cli/symbol.go:38-42`): `CTX_ARGUMENT_INVALID`, "not in this generation";
- the node's `content_hash` is compared with the snapshot file row only as an internal integrity
  check (`CTX_SOURCE_INTEGRITY` on mismatch), not as a normal refusal.

The answer's `binding` names the generation. Passing the `generation_id` from a callers answer's
`binding` is what makes the body match those rows exactly (graph answers carry ranges, [ADR-0017](../../../adr/ADR-0017-extraction-and-linking-coverage.md)).

**Size, without a fixed cap.** The 1 MiB chunk and the 7 MiB wire bound are wire-security bounds,
which the product's no-limits rule keeps: wire-security bounds and lossless pagination stay. Under them:

- a node read returns the whole node in one call whenever it fits one chunk, which is nearly every
  symbol; it is never clipped and never refused for size;
- a larger span (a generated file, a whole-module node) pages losslessly: `next_offset` plus a signed
  `next_cursor` that carries a cursor lease, the same convention as every other continuation
  (`services.go:960-963`, `graph/cursor.go:785-798`), so the generation cannot be collected between
  pages; the continuation stops at the node's end, not the file's;
- the CLI command loops the chunks itself and writes the whole span in one command, memory one chunk.

**Non-UTF-8.** Reuse `PlanChunk`: a chunk that is not valid UTF-8 is returned base64 with
`encoding: "base64"` and byte ranges, never with replacement characters (`chunk.go:28-33`,
`:122-124`). This beats the filesystem server ("always treats the file as UTF-8") and codebase-memory
(replaces malformed UTF-8 with U+FFFD).

**Security.** The read is by file id into the content store, never by a filesystem path, so there is
nothing to traverse. A `path` argument is only hashed into a file id and looked up in the snapshot's
manifest; a path that is not a member is "not in this generation", and no `open()` happens on it.
Capture already goes through `os.Root` (`docs/threat-model.md:35`; `docs/snapshots.md:11-13`), which
Go documents as refusing escapes by `..` or symlinks, and symlinks are excluded by default
(`docs/snapshots.md:32-33`). Only files the index captured can be read: nothing outside the tree, no
ignored files, no files under the data directory. The CLI must use the same snapshot path and never
the working tree.

**A client-specific size key, left out.** Raising Claude Code's 50,000-character inline threshold for
this one tool needs the `_meta["anthropic/maxResultSizeChars"]` key on the tool entry. That would put
one client's vendor-named key in the product's tool table. It is left out: exact-symbol reads are
small, and a whole-file read reaching the client as a saved file is acceptable.

## 6. What the design changes

- `codectx_read_source` without `session_id` reads one node (`node_id`) or one byte
  range (`file_id` or `path`, `offset`, `max_bytes`) from `generation_id` or the active generation,
  under a query lease; no receipt, no coverage, no issued-chunk row.
  - `internal/model/coverage.go`: request with optional session fields and the three selectors;
    `Validate` enforces exactly one mode; response with the optional `session {receipt, coverage}`
    block, `node_id`, `next_cursor`.
  - `internal/app/services.go`: a session-free read path (lease, `snapshot.View.Read`,
    `source.PlanChunk`, release); never through `coverage.Service.Read`, which requires a session.
    A node id resolves in the pinned generation; absent means "not in this generation". Continuations use a signed cursor with a cursor lease,
    bound to the node or range and the generation, ending at the node's end.
  - `internal/mcpserver/session.go`: route by mode; the `max_source_response_bytes` check applies to
    both modes.
  - CLI: `codectx source <node-id | file-id | path>` with `--offset`, `--max-bytes`, `--generation`,
    `--json`; prints the whole requested span by looping chunks, body written verbatim (as
    `internal/cli/context.go:198-209` does now).
  - Text: drop "only tool that returns source bytes" only where it implies a session; it remains the
    only tool that returns source. Update `internal/mcpserver/registry.go:72-73`,
    `internal/mcpserver/server.go:23-27`, `internal/mcpserver/limits.go:38-45`,
    `internal/mcpserver/explore.go:109-110`, `internal/mcpserver/session.go:68-72` and `:115-117`,
    `internal/model/query.go:319-323`, `internal/app/services.go:503-504`, `docs/threat-model.md:43`,
    `docs/context-sessions.md` (a short section: "Reading outside a session earns no coverage"),
    and `docs/queries.md` for the new CLI command.
  - `symbol_info`, `find_symbol`, `callers`, `callees` stay metadata only (option c declined).
  - Tool count stays 23.
- Tests: a node read returns exactly the node's bytes and the binding of the
  generation asked for; a node that moved between two generations is read at each generation's own
  range; a node absent from the asked generation is "not in this generation"; a range over one chunk pages to the
  node's end with no gap or overlap and survives a retention pass between pages; a non-UTF-8 file comes
  back base64 with byte ranges; a path not in the snapshot (including `../` and a symlinked path) is
  "not in this generation"; a session-free read creates no issued chunk and changes no coverage; a
  session read is unchanged.

## 7. Sources

Web:

- Model Context Protocol servers, Filesystem README — https://raw.githubusercontent.com/modelcontextprotocol/servers/main/src/filesystem/README.md
- GitHub MCP Server README (`get_file_contents`) — https://raw.githubusercontent.com/github/github-mcp-server/main/README.md
- GitHub REST API, Repository contents — https://docs.github.com/en/rest/repos/contents
- Sourcegraph MCP Server (7.6 docs) — https://7.6.sourcegraph.com/api/mcp.md
- Serena, `symbol_tools.py` (`FindSymbolTool`) — https://raw.githubusercontent.com/oraios/serena/main/src/serena/tools/symbol_tools.py
- Serena, `file_tools.py` (`ReadFileTool`) — https://raw.githubusercontent.com/oraios/serena/main/src/serena/tools/file_tools.py
- Serena, `tools_base.py` (`_limit_length`) — https://raw.githubusercontent.com/oraios/serena/main/src/serena/tools/tools_base.py
- codebase-memory-mcp README — https://raw.githubusercontent.com/DeusData/codebase-memory-mcp/main/README.md
- codebase-memory-mcp `src/mcp/mcp.c` (lines 30, 636-650, 12445-12600, 12915-12940: tool schema, containment guard, live-file read, 500-line clip) — https://github.com/DeusData/codebase-memory-mcp/blob/main/src/mcp/mcp.c
- code-review-graph user guide (no source-returning tool documented) — https://cdn.jsdelivr.net/gh/tirth8205/code-review-graph@main/docs/USAGE.md
- trace-mcp `get_symbol` (third-party listing, not the project's docs) — https://policylayer.com/tools/nikolai-vysotskyi-trace-mcp/get-symbol
- Claude Code, Tools reference ("Read tool behavior") — https://code.claude.com/docs/en/tools-reference
- Claude Code, MCP (output limits, `MAX_MCP_OUTPUT_TOKENS`, `anthropic/maxResultSizeChars`) — https://code.claude.com/docs/en/mcp
- Aider, Repository map — https://aider.chat/docs/repomap.html
- Anthropic, Writing effective tools for AI agents — https://www.anthropic.com/engineering/writing-tools-for-agents
- MCP specification 2025-06-18, Pagination — https://modelcontextprotocol.io/specification/2025-06-18/server/utilities/pagination
- MCP specification 2025-06-18, Tools — https://modelcontextprotocol.io/specification/2025-06-18/server/tools
- MCP specification 2025-06-18, Resources — https://modelcontextprotocol.io/specification/2025-06-18/server/resources
- LSP 3.17, Goto Definition — https://raw.githubusercontent.com/microsoft/language-server-protocol/gh-pages/_specifications/lsp/3.17/language/definition.md
- LSP 3.17, DidOpenTextDocument — https://raw.githubusercontent.com/microsoft/language-server-protocol/gh-pages/_specifications/lsp/3.17/textDocument/didOpen.md
- LSP 3.17 specification (Position, LocationLink, position encodings) — https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification/
- Cymulate, EscapeRoute: CVE-2025-53109 and CVE-2025-53110 — https://cymulate.com/blog/cve-2025-53109-53110-escaperoute-anthropic/
- The Go Blog, Traversal-resistant file APIs (`os.Root`, Go 1.24) — https://go.dev/blog/osroot

Not used: Cursor's read-file limits are documented only in forum posts that disagree with each other,
so they are left out.

Code and docs read:

- `internal/mcpserver/registry.go:22-94`, `server.go:16-44`, `session.go:60-161`, `limits.go:15-80`, `explore.go:103-121`
- `internal/coverage/read.go:1-239`
- `internal/model/coverage.go:59-160`, `ids.go:148-171`, `query.go:92-110`, `query.go:300-345`, `query.go:422-431`, `validate.go:37-44`, `facts.go:122-124`, `provider.go:159-161`
- `internal/app/services.go:225-250`, `:490-532`, `:715-747`, `:955-1000`
- `internal/graph/cursor.go:785-798`
- `internal/snapshot/view.go:1-260`
- `internal/source/chunk.go:1-140`
- `internal/storage/sqlite/retention.go:95-100`; `internal/index/retention.go:20-60`
- `internal/config/config.go:207`, `:239`, `:494`, `:565`, `:582`, `:599`, `:652`
- `internal/cli/context.go:130-225`, `internal/cli/symbol.go:1-60`
- `docs/context-sessions.md`, `docs/snapshots.md`, `docs/threat-model.md:16-43`
