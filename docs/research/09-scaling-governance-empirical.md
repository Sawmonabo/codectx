# Scaling, memory governance, call-join coverage, reads/writes (2026-09-13, this machine: 16 cores, 47 GiB)

Engine: engine 4.0.627, JDK 21.0.12. All numbers from /usr/bin/time (%M = peak RSS of the largest
process in the tree).

## 1. Scaling on real repositories, default JVM heap (25% of RAM ≈ 12 GB)

| Corpus | Lang | LOC parsed | Files | parse wall | parse RSS | cpg.bin | export wall | export RSS | CSV | CALL / CDG / REACHING_DEF rows |
|---|---|---|---|---|---|---|---|---|---|---|
| codectx | Go | 37.6k | 126 | 4.7 s | 2.2 GB | 4.9 MB | 2.0 s | 0.7 GB | 49 MB | 35k / 61k / 239k |
| golang.org/x/tools v0.49.0 | Go | 239k | 1,283 | 11.9 s | 7.2 GB | 14 MB | 4.5 s | 1.2 GB | 145 MB | 111k / 101k / 747k |
| redglass (Python monorepo) | Python | 506k | 2,089 | 25.8 s | 7.0 GB | 98 MB | 27.5 s | 5.2 GB | 1.27 GB | 5.0M / 413k / 4.1M |
| r3/app (Meteor TS/JS) | TS/JS | 1.0M | — | 166 s | **12.2 GB, then engine crash** | 86 MB (pre-overlay) | — | — | — | — |
| r3/app/imports | TS/JS | 22k | — | 9.7 s | 3.0 GB | — | — | — | — | 77k / 55k / 501k |
| r3/app/lib | TS/JS | 20k | — | 5.7 s | 1.1 GB | — | — | — | — | 25k / 24k / 184k |
| r3/app/both | TS/JS | 81k | — | 17.3 s | 5.2 GB | — | — | — | — | 177k / 81k / 1.08M |
| r3/app/client | TS/JS | 324k | — | 108 s | 8.9 GB, **engine crash** | 46 MB | — | — | — | — |
| scip-go on x/tools (for contrast) | Go | 239k | 1,283 | 7.3 s | 0.4 GB | 21 MB index | — | — | — | — |

The r3 crash is inside the engine (`ObjectPropertyCallLinker` → `AssignmentMethods.source`:
"Assignment statement with 3 arguments"), reproduced with vendored amcharts excluded, so it is a
code-shape bug in the engine's JS/TS frontend, not a memory failure. A crashed unit leaves a pre-overlay cpg.bin
that cannot be exported (schema violation in ReachingDefPass). Per-directory bisect result is in
`bisect.txt` (appended below when finished).

## 2. Memory governance: the JVM heap cap is honored and lossless

`JAVA_OPTS=-Xmx<N>` reaches the frontend JVM (the parse command's own `-J-Xmx` does not; that JVM only
orchestrates). Results on x/tools (239k Go LOC), cpg.bin byte-identical (14,308 KB) in every
successful run:

| Cap | parse wall | peak RSS | result |
|---|---|---|---|
| default (~12 GB) | 11.9 s | 7.2 GB | ok |
| 3 GB | 9.7 s | 2.9 GB | ok |
| 1.5 GB | 9.7 s | 1.9 GB | ok |
| 800 MB | 10.9 s | 1.1 GB | ok |
| 500 MB | 13.0 s | 1.0 GB | ok |

redglass (506k Python LOC): 2 GB cap → ok, 37 s, 3.9 GB RSS, identical cpg; 1 GB cap →
`OutOfMemoryError: Java heap space`, exit 1, no cpg (fails closed, never partial).

Conclusion: the multi-GB figures are JVM default sizing, not analysis need. Real need is roughly
2 MB heap per 1k LOC (Go) to 3–4 MB per 1k LOC (Python). The governor can set the cap from the
unit's byte count and the configured envelope, retry once at 2x on OOM, and report the unit as
`failed: memory` beyond that. Nothing is silently dropped at any cap.

## 3. Two-phase parse works in 4.0.627
The parse command with `--language golang --overlaysonly --output <existing cpg.bin>` applies overlays
to an existing graph in place (0.68 s when already applied). Report 06's NPE is a 4.0.100 defect
fixed by 4.0.627. Not needed for the design, but it means a cached frontend graph can have
overlays re-applied without re-parsing.

## 4. Unit boundaries
- Go: `gosrc2cpg` requires a `go.mod`; a subtree without one yields an empty graph (68 KB, no
  methods). Unit = module. Cross-module calls survive as external stubs with full names (07).
- TS/JS: any directory works; unit = package.json/tsconfig project. Smaller units contained the
  engine crash to one unit.
- Python: any directory; unit = package. Rust: Cargo package with cargo on PATH.

## 5. "Who calls what" from tree-sitter call sites + SCIP, measured on codectx

8,325 Go call sites (go/ast, non-test files). SCIP occurrence present at the callee identifier:

| Kind | sites | resolved by SCIP | notes |
|---|---|---|---|
| `pkg.Func()` / `x.Method()` selectors | 4,218 | 4,116 (97.6%) | misses: `err.Error()` on the builtin `error` interface (70), Windows-only files not built on Linux (26) |
| bare identifiers `f()` | 4,048 | 2,238 | the 1,810 misses are Go builtins and conversions: len 683, string 408, append 246, int64 156, make 103, uint64 78 … none is a user function |
| other (`fn()()`, literals) | 59 | — | call through an expression; no static callee |

Every miss is either a builtin/conversion (not a call to user code), a build-configuration gap
(SCIP indexes one GOOS/GOARCH), or a call through a value. Calls through a function value
(`f := helper; f(2)`) resolve to the *local* `f`, which is the honest answer; the engine's CALL for
the same site also names no method. 84 sites resolved to `local` symbols this way. Coverage of
user-code calls is effectively complete; the residual gaps are labelled, not hidden.

Richness check against the engine on the same corpus: the engine has 10,366 non-operator CALL
nodes, 196 with unresolved full names, all STATIC_DISPATCH; the extra count is test files, builtins
and conversions. Nothing the engine's CALL edge carries is missing from the join except the argument
subtree, which we do not publish as a fact.

## 6. Reads / writes are derivable from the CPG
codectx export: 5,542 `<operator>.assignment` CALL nodes; 3,768 have an IDENTIFIER as the
written argument, 1,182 a field access; 3,660 of the identifier targets have a REF edge to their
declaration (LOCAL / MEMBER / PARAMETER). So `writes` = assignment target with a REF, `reads` =
other identifier uses with a REF, both `static_analysis` precision, produced by the dependence
provider from the same export. Neither SCIP (no WriteAccess from any indexer) nor tree-sitter
alone (no resolution) can produce them; SCIP+tree-sitter could produce a `syntax`-precision
approximation, which is not needed once the dependence provider exists.

## 7. Bisect of the crashed TypeScript unit (r3/app/client, 324k LOC)
Every subdirectory parses on its own under a 4 GB cap; only the combined tree crashes the
engine's cross-file `ObjectPropertyCallLinker` pass:

| subdir | LOC | wall | RSS |
|---|---|---|---|
| imports | 135k | 23.8 s | 3.0 GB |
| views | 120k | 19.4 s | 3.1 GB |
| components | 41k | 9.7 s | 1.9 GB |
| lib | 20.5k | 38.2 s | 2.6 GB |
| styles, plugins, unitTests, collections, utils, definitions, startup | <4k each | 1.4–3.7 s | 0.2–0.6 GB |

Eleven units, about 105 s total, peak 3.1 GB, zero failures, versus one unit at 108 s, 8.9 GB
and a crash. Unit sizing is the governor's lever for both memory and engine-bug blast radius; the
cost is cross-unit call targets landing on external stubs (aliased by full name) and cross-unit
object-property linking not happening. That loss is reported per unit as `scope: partial-project`.
