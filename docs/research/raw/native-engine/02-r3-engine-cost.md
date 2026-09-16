# Native-engine research, raw evidence — measured engine cost on r3 (the ranking input for the phase order)

Source: the program ledger entry of 2026-09-16 16:10 EDT (r3-3 result),
binary 62976df, run under `systemd-run`, 13,222 files, generation 3, rc=0, wall 31:38.10.
Quoted verbatim below so the figures in the report are traceable without re-reading the program ledger.

```
- 2026-09-16 16:10 EDT — r3-3 RESULT (binary 62976df, `systemd-run`, clean shell, corrected recorder): rc=0, 
wall 31:38.10, 13,222 files, generation 3, health degraded. NO STALL: probe worst 73 ms; one burst of ten 
kernel bounce-buffer lines at 15:40:14 (evidence only); MemAvailable floor 32.8 GB (of 47); product VmPeak 
7.7 GB; JVM peaks 9,263 / 8,752 / 8,186 / 8,167 / 6,563 / 4,845 MB (r3-1: 17,651 / 17,576 / 12,502 …) — the 
heap-from-need rule halved the engine's footprint. TIME BREAKDOWN: startup 7 s; snapshot 7 s; structural 
parse + lexical + first seal 15:34:38→15:39:36 = 4:58 (r3-1 5:09; two parser workers by the fixed setting 
the machine-sizing fix removes); SCIP units + capability fold + compaction + adjacency + activation + retention + collection 
15:39:36→15:40:54 = 1:18; dependence 15:41:02→16:04:30 = 23:28 (74%; r3-1 19:47) of which 
`pkg:javascript:app` 15:43:49→~16:04:10 ≈ 20:20 (64% of the run: parse attempt 1 3:23 → exit 1, attempt 2 
3:22 → "reproducible backend crash", subdivided into 9 children run ONE AT A TIME by 
`max_concurrent_heavy_analyzers = 1`, 15:50:34→16:04:13) and the other ten units ≈ 3 min; final adjacency + 
activation ≈ 1 min. SLOWER than r3-1 by 4:41 because the two `app` parse attempts now run to the linker crash 
(3:20 each; in r3-1 they died at 52 s on the Cyrillic path) and the nine children are serialised. 
CAPABILITIES: 8 fresh, 5 partial — every engine capability partial because 
`pkg:java:QA/SeleniumWebdriver/TestngExtentFramework` failed `the analysis exported no method for a unit that 
has source`; the same Java project's precise unit failed OUTPUT_INVALID and NINE of ten TypeScript precise 
units failed UNAVAILABLE (0 records) — yet `precise_definitions/implementations/references` read FRESH: the 
per-provider fold reported fresh with ten of eleven planned units failed, which is the silent capability 
reduction Section 30.1 forbids. The `app` subdivision is not named in the fold either (details name one scope 
only). The crash's `pass` was recorded empty. All of it routed to the capability-state fix. 
(Ledger housekeeping lines omitted.)
```

## Extracted figures

| figure | value | note |
|---|---|---|
| total run wall | 31:38.10 | r3-3 |
| dependence (engine) phase | 15:41:02 → 16:04:30 = 23:28 | 74% of the run |
| `pkg:javascript:app` alone | 15:43:49 → ~16:04:10 ≈ 20:20 | 64% of the whole run |
| structural parse + lexical + first seal | 15:34:38 → 15:39:36 = 4:58 | tree-sitter tier |
| SCIP units + fold + compaction + adjacency + activation + retention | 15:39:36 → 15:40:54 = 1:18 | precise tier |
| the other ten engine units | ≈ 3 min | all non-JavaScript |
| `app` parse attempt 1 / attempt 2 | 3:23 → exit 1 / 3:22 → "reproducible backend crash" | deterministic |
| `app` subdivision | 9 children, serialised by `max_concurrent_heavy_analyzers = 1`, 15:50:34 → 16:04:13 = 13:39 | the recovery path |
| JVM peaks | 9,263 / 8,752 / 8,186 / 8,167 / 6,563 / 4,845 MB | r3-1 was 17,651 / 17,576 / 12,502 |
| product VmPeak | 7.7 GB | whole product, all tiers |
| capabilities | 8 fresh, 5 partial | every engine capability partial |

## The JavaScript crash (the second ranking input)

Source: the unexportable-unit fix report, unit 3b bisect (pinned payload, product argv, one JVM at a time).

- Smallest reproducing set is **two files**: `client/lib/gojs/go.js` (892 KB bundled library) and
  `client/collections/attackPath.js` (766 bytes, one statement:
  `this.vulnAnalysisVulns = new Mongo.Collection("vulnAnalysisVulns");`). Neither reproduces alone.
- Parse: `ERROR ObjectPropertyCallLinker  Pass io.joern...jssrc2cpg.ObjectPropertyCallLinker failed`
  / `java.lang.RuntimeException: Assignment statement with 3 arguments` at
  `...operatorextension.nodemethods.AssignmentMethods$.source(AssignmentMethods.scala:15)`.
- Export of the graph the failed parse still wrote: `ERROR ReachingDefPass
  flatgraph.SchemaViolationException: OUT edge with label REF to an adjacent METHOD is mandatory,
  but not defined for this METHOD_REF node`, then `NoSuchElementException: next on empty iterator`.
- The trigger is **ordinary, legal JavaScript** (assignment to a field of `this` at module top level
  lowered to a three-argument assignment node that the linker's accessor rejects), so it cannot be
  pre-screened without refusing real source.
