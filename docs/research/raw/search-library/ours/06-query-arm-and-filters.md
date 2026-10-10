# 06 — How the query arm reproduces `codectx search`, and the filter semantics

All file:line references are to commit 16dfef6. The harness is `source/cmd/tbquery/main.go` and `source/internal/search/tbench.go`.

## Composition
The query composition is reproduced exactly (`app.openStack` in query mode):
- the store opened read-only with the same options (`internal/app/compose.go:669`);
- the content store opened for reading (`:662`);
- the signer read, not minted (`:736`);
- the spools (`:744`) and the leases;
- `search.New` with the configured resources and cursor TTL (`:1202`).

The content store is the real run's, so hydration reads real bytes. A read-only reader is not "continuable" (`internal/storage/sqlite/query.go:155`), so, as in production for a read-only open, the continuation spool is still written but no lease row is taken.

## The timed call: `Service.BenchLexical`
It follows `Service.Search`'s first-page path (`internal/search/search.go:179`):
1. Validate the request.
2. `newHitFilter` (`:930`).
3. `PinGeneration` of the active generation (`:216`).
4. `filter.pin` (`:241`).
5. The lexical tier (`internal/search/lexical.go:174`). Every emitted document is kept or dropped by `filter.keep` and `matchesKinds`, exactly as `lexicalCandidates` does (`search.go:832`), then added to the deduplication collector.
6. `deduped()`, then the `firstPage` walk (`search.go:291`): the page heap, the raw continuation spool, hydration from the content store (`internal/search/rank.go:447`), and the cursor sign.

Each query runs in a fresh process: one cold run, then N warm runs. The open is timed apart (`open_ms`). The continuation spool is released after each run, outside the timing.

## Recorded fields
- `hits`: the matching documents that pass the filters.
- `page`: the first 200 matching documents ordered by `cmpScored` (`rank.go:75`), with no deduplication. It is computed by `BenchDocPage`, outside every timing, under the same filters. This document-level ordering is the one the other arm's product mode reproduces.
- `served_hits` / `served_page`: production's answer after deduplication. Deduplication is by node id, else by path and start byte (`rank.go:116`). Every filesystem chunk carries its file's node id (`internal/provider/filesystem/chunk.go:289`), so chunks fold into one hit per file. On the smoke, "go" has 1052 documents and 1045 served hits.
- `filter`: `{"kinds":[...],"path_prefixes":[...]}`.

## Filter semantics (`--kind K`, `--path-prefix P`, both repeatable)
- `--kind` sets `SearchRequest.Kinds` (`internal/model/query.go:247`), which is what `codectx search --kind` sets (`internal/cli/search.go:51`–`52`).
  - Matching is exact equality with the document's kind, OR'ed across the given kinds (`internal/search/exact.go:255`).
  - A kind that is not a known node kind is refused by request validation.
- `--path-prefix` sets `SearchRequest.Paths`. The CLI has no flag for it; the MCP search tool takes the whole `SearchRequest` (`internal/mcpserver/explore.go:91`).
  - Normalization (`search.go:930`): trim spaces, turn `\` into `/`, drop a trailing `/`.
  - Refusal: an empty, absolute, unclean or `..` path is refused with CTX_ARGUMENT_INVALID, "is not a root-relative repository path" (`:936`).
  - Matching is by whole path segments: a document is kept when its path equals the prefix or starts with prefix + "/" (`:959`). So `doc` keeps `doc/x.go` but not `docs/x.go`.
  - Several prefixes are OR'ed.
- Kind and path combine with AND.
- Both filters apply on the lexical tier before deduplication and paging, so they change `hits`, the deduplicated set and the page.

Smoke on spf13/cobra, query "command flags" (631 documents unfiltered):

| Filter | Documents |
|---|---|
| `--kind function` | 100 |
| `--path-prefix site` | 12 |
| `--kind function --kind method --path-prefix doc` | 24 |
| `--path-prefix /abs` | refused with CTX_ARGUMENT_INVALID |

## Other smoke results (5 warm runs)
| Query | Hits | Cold (ms) | Warm (ms) |
|---|---|---|---|
| go | 1052 | 26.0 | 19–31 |
| "command flags" | 631 | 25.0 | |
| "persistent flags" (phrase) | 11 | 6.5 | |
| 0400 | 1 | 4.5 | |

Warm runs vary widely on the shared host.
