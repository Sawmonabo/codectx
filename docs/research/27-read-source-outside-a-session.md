# Reading source outside a context session: how code-graph servers serve one symbol's bytes

Research note for the codectx product owner, behind [ADR-0018](../adr/ADR-0018-read-source-outside-a-session.md).
Research date 2026-10-09. The full findings, the tool comparison and every source are in
[raw/read-source/findings.md](raw/read-source/findings.md); sources are cited here as [n] from §5.

## 1. The question

`codectx_read_source` serves source only inside a context session, with a receipt. An agent that has a graph answer
naming `Authorize` at a file and range must open a session to read those 30 lines, or read the working tree, which may
no longer match the generation that answered. Should the tool also read an exact node or byte range from a pinned
generation, with no session, no receipt and no coverage credit, and how should size, encoding and paths be handled?

Of the four reasons reads are session-bound in the code, only one needs a session: receipts and coverage. Pinning is
done for every query by a lease; the chunk and wire bounds do not come from the session; and "one tool returns
source" constrains which tool, not whether a session is open.

## 2. What the field does

- **Exact span reads with no ceremony are the norm.** Code-graph and code-host servers read one symbol or one line
  range directly, by symbol, path and range, or revision [1][2][3][4].
- **The careful ones pin a version.** Reads by commit or revision are consistent with the answer that named them [1][2].
  One server applies indexed line numbers to the live file, so after an edit the "symbol" can be the wrong lines [4].
  The index's content store is the pinned version, so the node's range and its bytes share one content hash.
- **Size handling is where they go wrong.** A 500-line clip, the first 200 lines of a large file, or nothing at all
  over a character limit [2][3][4]. Paging with an explicit continuation is right; an agent client's own file read
  pages with a notice [5]. Agent-tool guidance asks for range selection and pagination and for high-signal answers [6].
- **Encoding.** A filesystem server treats every file as UTF-8; one code-graph server replaces malformed bytes with
  U+FFFD [4][7]. The index's chunk planner already returns invalid UTF-8 as base64 with byte ranges.
- **Paths are where they get hurt.** A reference filesystem server had two path-escape vulnerabilities in 2025: a
  string-prefix containment check, and a symbolic-link check that validated the link instead of its target [8][9].
- **Resources.** The protocol's resources are application-driven, so a model cannot reliably pull one, and a resource
  read takes no range [10].
- **Client limits.** One agent client warns at 10,000 tokens of tool output and stores any text result over 50,000
  characters as a file [11]. An exact symbol is far below that; a 1 MiB chunk reaches such a client as a stored file.

## 3. Findings

1. Read by `node_id`, or by `file_id`/`path` with `offset` and `max_bytes`, from `generation_id` or the active
   generation, pinned by a query lease; no receipt, no coverage, no issued chunk. One tool still returns source.
2. Resolve the node in the pinned generation: with name-path identity the same node id can sit at different ranges in
   different generations; an absent node is "not in this generation".
3. Return a node whole when it fits one chunk; page anything larger losslessly with a signed cursor holding a lease,
   ending at the node's end. Never clip.
4. Return invalid UTF-8 as base64 with byte ranges.
5. Read by file id from the content store only; a `path` is hashed to a file id and looked up in the manifest, never
   opened.
6. Leave the graph and symbol tools metadata only, and add no client-specific size key.

| Option | Verdict |
|---|---|
| Session reads only | the agent leaves the index or opens a session for a few lines |
| **Reads outside a session, no credit** | **matches the field; reuses the snapshot view and chunk planner; exact by content hash** |
| A body flag on graph and symbol tools | every graph answer becomes a source answer under the larger bound |
| Snapshot files as protocol resources | host-driven and rangeless; a later addition, not a replacement |

## 4. Not tested hands-on

No server was run; the comparison rests on each tool's source and documentation, read during the research. One
read-tool listing is a third-party catalogue page, not the project's own documentation, and is marked so in the raw
findings.

## 5. Sources

1. *GitHub MCP Server README*, https://raw.githubusercontent.com/github/github-mcp-server/main/README.md
2. *Sourcegraph MCP Server*, https://7.6.sourcegraph.com/api/mcp.md
3. *Serena symbol_tools.py*, https://raw.githubusercontent.com/oraios/serena/main/src/serena/tools/symbol_tools.py;
   *tools_base.py*, https://raw.githubusercontent.com/oraios/serena/main/src/serena/tools/tools_base.py
4. *codebase-memory-mcp mcp.c*, https://github.com/DeusData/codebase-memory-mcp/blob/main/src/mcp/mcp.c
5. *Claude Code, Tools reference*, https://code.claude.com/docs/en/tools-reference
6. *Writing effective tools for AI agents*, https://www.anthropic.com/engineering/writing-tools-for-agents
7. *Filesystem MCP server README*, https://raw.githubusercontent.com/modelcontextprotocol/servers/main/src/filesystem/README.md
8. *EscapeRoute: CVE-2025-53109 and CVE-2025-53110*, https://cymulate.com/blog/cve-2025-53109-53110-escaperoute-anthropic/
9. *Traversal-resistant file APIs*, https://go.dev/blog/osroot
10. *MCP specification 2025-06-18, Resources*, https://modelcontextprotocol.io/specification/2025-06-18/server/resources
11. *Claude Code, MCP*, https://code.claude.com/docs/en/mcp
