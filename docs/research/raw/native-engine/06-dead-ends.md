# Native-engine research, raw evidence — dead ends and negative results (recorded so they are not re-explored)

Recorded per the user's persistence requirement: a dead end is recorded as a dead end, with why.

| candidate route | verdict | why, with source |
|---|---|---|
| Reuse the engine incrementally (per-file CPG update) | **dead** | No incremental CPG API. Maintainer: "No, it does not right now" (joern#5757); feature request joern#5865 open since 2026-03-08. `--overlaysonly` re-application corrupts graphs. `docs/research/11-incremental-joern.md`, `14-incremental-synthesis.md` §2. |
| Splice per-file engine graphs together | **dead** | No merge, no append, no per-file export; TypeScript file-only parses produce 19% **wrong** `METHOD_FULL_NAME` call edges, which aliasing cannot repair and which must not be published. `11-incremental-joern.md` §4.3. |
| Get CDG/REACHING_DEF out of the engine more cheaply (`--repr=pdg`, `joern-slice`) | **dead** | `--repr=pdg` is not implemented for CSV or GraphML; `joern-slice` cannot emit CDG; DOT views are display-filtered projections. `docs/research/04` §2, `00-synthesis.md` §2 row 8. |
| Drop the engine and take `calls` from SCIP alone | **dead as a whole-product move** | All six indexers were measured: call-site and function-value references carry identical roles, so SCIP alone cannot distinguish a call from a reference. `00-synthesis.md` §2 row 1, `docs/research/08`. |
| Take `reads`/`writes` from SCIP role bits | **dead** | No indexer sets `WriteAccess`, anywhere. `00-synthesis.md` §0a, `providers-dependence.md`. |
| `stack-graphs` / `tree-sitter-graph` as the CFG source | **dead** | Name resolution and a graph-construction DSL; no CFG, no post-dominators, no control dependence. `docs/research/05` §3. |
| Fraunhofer AISEC `cpg` as the backend | **dead for this product** | Same JVM profile the product is leaving. `00-synthesis.md` §0 last row. |
| CodeQL | **dead** | Not cheaper (64 GB / 8 cores above 1M LOC), needs a build for compiled languages, extractors not open. `docs/research/04` §3.5. |
| Lower `--max-num-def` to cut engine cost | **dead** | Exceeding it zeroes a method`s REACHING_DEF with only a stderr WARN; the uncapped rerun showed the higher limit costs 23% time and 3% memory and removes every skip. `00-synthesis.md` §2 row 4, `10-round3` §9a. |
| Split a JavaScript/TypeScript unit to make it cheaper | **dead** | 46% of calls resolved to internal methods survive a four-way split; the ruling is never split a tsconfig project except as crash recovery. `10-round3` §8. |
| Shrink a proof run / cap the product to fit the host | **forbidden** | Host workarounds are not permitted; a stall is a product defect. (Standing rule.) |
