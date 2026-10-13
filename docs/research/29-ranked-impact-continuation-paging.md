# Ranked impact continuation: the cost of a later page

Measurement record behind the paging claim in [performance.md](../performance.md) Section 3.3 and the ranked impact
continuation in `internal/graph` (`Engine.serveRankedImpact`, `pagination.Spools.OpenAt`). It compares a continuation
that rewrites the unserved remainder on every page with one that seeks to the page its cursor names. Measured on the
host of performance.md Section 1; the first two tables were recorded on 2026-09-15, the last on 2026-10-13.

## 1. The question

The whole-compile scale proof (`internal/context`, `TestTheStreamedCompilePeaksOnTheRunBufferAtEitherScale`) publishes
a fan-out fixture of N leaf entities, compiles a plan over it and pages the persisted manifest back. Its compile time
rose faster than N. The question was where: in the context compile's own external sorts, or in the graph walk it reads.

## 2. Copying continuation (each later page rewrites the remainder)

Compile under the default admission (`resources.query_memory_bytes`, 32 MiB), where these sorts never fill their 8 MiB
run buffer and nothing spills, and under a 1 MiB admission, where both sizes spill:

| N (leaves) | Fixture publish | Compile | Page latency, persisted manifest (200 rows/page) |
|---|---|---|---|
| 500 | — | 105 ms | 0.27–0.56 ms |
| 1 000 | — | 152 ms | 0.50–0.62 ms |
| 2 000 | — | 247 ms | 0.52–0.70 ms |
| 4 000 | — | 536 ms | 0.57–1.22 ms |
| 8 000 | — | 1.272 s | 0.51–1.61 ms |
| 16 000 | — | 4.116 s | 0.48–1.64 ms |
| 2 000 (1 MiB admission) | 2.8 s | 469 ms | 0.48–0.88 ms |
| 4 000 (1 MiB admission) | 6.8 s | 849 ms | 0.45–1.07 ms |

The per-doubling compile ratios are 1.45, 1.62, 2.17, 2.37, 3.24: increasing, so the local exponent at the top of the
range (8 000 to 16 000) is log2 3.24, about 1.70.

**Cause.** The continuation of a ranked impact walk served each later page by opening the ranked spool from its start
and copying every record after the page into a fresh spool, so a page cost O(records remaining) and a walk of N entries
read to exhaustion at 200 rows a page cost on the order of N²/400 record reads and writes. A CPU profile of the
500/1 000/2 000 and 4 000/8 000/16 000 runs attributed 0.69 s of the 1.13 s spent in `Engine.walkImpact` to
`serveRankedImpact`, 0.55 s of it in `pagination.Spools.Open` (the re-read and the re-copy), a share that rose with N.
Nothing in `internal/context` was quadratic.

## 3. Seeking continuation (each later page reads only itself)

The spool is written once, by the page that settles the order; every later page seeks to the byte offsets its cursor
carries and reads only its own records (`pagination.Spools.OpenAt`, `RankOffset`/`PairOffset`). On the 20 101-node
deadline-split fixture at 200 rows a page (`graph.TestADeadlineSplitWalkAlwaysAdvances`):

| Page | Copying continuation, latency | Seeking continuation, latency | Seeking continuation, spool bytes read |
|---|---|---|---|
| 3 | 18.9 ms | ~0.39 ms | 88 046 |
| 50 | 10.7 ms | ~0.39 ms | 88 046 |
| 100 | 1.9 ms | ~0.39 ms | 88 046 |

Nothing is written after the spill. The copying latency falls with the shrinking remainder; the seeking latency is flat.

## 4. The compile curve with the seeking continuation

2026-10-13, 1 MiB admission, `GOMAXPROCS=4`, `GOMEMLIMIT=3GiB`, on a host shared with other work:

| N (leaves) | Fixture publish | Compile | Page latency (200 rows/page) | `scope-cand` peak / spilled runs |
|---|---|---|---|---|
| 2 000 | 1.77 s | 788 ms | 0.32–0.51 ms | 927 records / 3 |
| 4 000 | 4.05 s | 1.437 s | 0.33–0.54 ms | 927 records / 5 |
| 8 000 | 10.62 s | 3.035 s | 0.32–0.89 ms | 927 records / 9 |
| 16 000 | 30.99 s | 7.819 s | 0.35–1.00 ms | 927 records / 18 |

The per-doubling ratios are 1.82, 2.11 and 2.58: the compile is still superlinear at this scale. This record does not
attribute the remaining curvature; a profile at these sizes is what would. The 1 MiB rows of Section 2 and these
differ in worker count and host load, so the two tables are not a timing comparison; the ratios within each table are.
