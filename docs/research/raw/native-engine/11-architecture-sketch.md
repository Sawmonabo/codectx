# Native-engine research, raw evidence — architecture sketch for a native dependence engine

Design notes behind §5 of the report. Every requirement traces to a product document; every sizing
figure traces to a raw file in this directory.

## The requirements this must meet (not negotiable, from `00-synthesis.md` §8 and `providers-dependence.md`)

no caps · no subdivision · memory proportional to a function · parallel across cores · streamed into
the store · incremental per file · no lowered analysis limits · no omitted fact families.

## The pipeline

```
per file (the unit of caching and invalidation)
  tree-sitter CST                 -- built in the structural provider's own worker subprocess, which
                                     owns every native object; the analysis runs INSIDE that worker,
                                     because nothing but fact frames crosses the wire
  └─ per function (the unit of WORK and of MEMORY)
       normalise            CST subtree -> a fixed statement/expression vocabulary   [per-language]
       CFG                  over the normalised vocabulary                           [shared]
       post-dominators      reversed, exit-augmented CFG; Cooper-Harvey-Kennedy       [shared]
       CDG                  dominance frontier of the reversed CFG (Ferrante 3.1.1)  [shared]
       def/use              write-site enumeration + read enumeration                [per-language]
       reaching defs        forward may-analysis, bitset worklist                     [shared]
       emit                 facts, in projection order, straight to the store        [shared]
calls
  precise index (SCIP) where a profile applies      -> compiler precision
  tree-sitter name heuristics otherwise             -> syntax precision, may_refer_to + candidates
  (unchanged from today; the native engine does not own `calls`)
```

The split is deliberate and is the whole design: **the four intraprocedural families are computed
per function; `calls` is not computed here at all.** That is the split the parity data already draws
(`03-parity-and-oracle.md`: CDG 99.7% / REACHING_DEF 99.9% survive subdivision, resolved calls 46% do
not) and that the file-locality data confirms from the other side (`05-file-locality.md`: the engine
itself reproduces 100% of CDG and REACHING_DEF from a single file for Python, TypeScript, C and Java).

## The unit of work and parallelism

| | engine today | native |
|---|---|---|
| unit of work | frontend-native project (a whole `tsconfig`, a whole Go module, the whole repo for C) | **one function** |
| unit of caching | project, keyed on the input closure | **one file**, keyed on blob hash + pass version (the model the structural tier already uses) |
| parallelism | `max_concurrent_heavy_analyzers = 1` by default; units serialised by summed reservations | one worker per core over a bounded function queue |
| memory | heap cap sized from unit bytes + per-family resident allowance (C/C++ 2.6 GB, Python 1.9 GB) + helper allowance; JVM peaks 4.8–9.3 GB on r3-3 | CST for one file + CFG for one function; a bound the product sets, not one it discovers |
| failure blast radius | a project. On r3 this meant two 3:20 parse attempts and nine serialised children, 20:20 total | one function, named, skipped, reported |
| on-disk intermediate | Neo4j CSV (0.65–4.95 GB measured) staged into a 256 MiB-cached SQLite scratch at 6.6× the export | **none** |

Memory per worker: the dominant term is the file's CST, which the structural provider already holds
and bounds today; the CFG and bitsets are O(statements in one function). No worker holds a project,
so nothing in the design can grow with repository size — which is what makes "no caps, no
subdivision" a property rather than a policy.

## Incrementality

A changed file invalidates that file's dependence facts and nothing else. This is the model the
structural tier already runs (blob hash + grammar version) and it is what the engine cannot offer at
any price (`06-dead-ends.md`, row 1). The measured waste it removes: today one edited file re-runs a
whole unit — 16–28 s for a 240k-line Go module, 53–65 s for a 500k-line Python tree — to change about
**one fact row in ten thousand** (`docs/research/14-incremental-synthesis.md` §1).

## The differential oracle

**It mostly exists already.** The importer computes an engine-id-independent semantic key per fact
(label, owning method full name, file, source operator, target name, ordered byte ranges, and for a
relation both endpoints' published identities), streams the key set to a sorted file and diffs two
sets in one merge pass (`03-parity-and-oracle.md`). A native engine is a **second producer feeding an
existing key algebra**, not a new comparison framework.

What remains:
1. The corpus — pinned by commit in Task 21, which `00-synthesis.md` §8 already names as "the
   differential oracle for any future native engine".
2. **The band.** Two engine runs over the same unmodified tree differ by ~0.01%
   (`10-round3` §9b), and `providers-dependence.md` makes a claim of equality between two engine runs
   a defect. So the oracle establishes the band from two engine runs *first*, then judges the native
   run against the band. A native-vs-engine diff inside the band is not a finding.
3. Per-family thresholds, because the families diverge for different reasons: CDG divergence is a
   normalisation difference, REACHING_DEF divergence is a def/use difference, and `reads`/`writes`
   divergence is a resolution difference. One aggregate number would hide all three.

## The precision label

`internal/model/facts.go:94-98` offers `compiler`, `language_server`, `static_analysis`, `syntax`,
`heuristic`. The engine stamps `static_analysis`. The honest label for a CST-derived CFG/CDG/def-use
whose endpoints are resolved by name heuristics is **`syntax`** (`docs/research/05` §0). Where the
endpoints come from a precise index the *calls* endpoint is `compiler` and the dependence edge is
still `syntax`.

This is a product-visible downgrade for the languages the engine covers today, and it is the single
largest non-engineering cost in the plan. The mechanism to carry two sources at two precisions
already exists — evidence `Detail` strings are built exactly this way (`cdg`, `reaching_def`), so a
native detail is an existing pattern, not new surface.

## What would still need the engine, and for how long

| fact family | after the native engine | why |
|---|---|---|
| `control_depends_on` | native, all nine languages | strictly per-function; file-locality proven |
| `data_flows_to` | native, all nine languages | intraprocedural; file-locality proven |
| `reads` / `writes` | native **write-site enumeration**; target resolution only as far as a binding source carries it | no SCIP indexer sets a write role; syntax cannot resolve an assignment target (`10-reaching-defs-and-readswrites.md`, Unit 2) |
| `calls` | **native, in two later phases**: a static name join for the four frontends the engine gives no type recovery (C/C++, Go, Rust, Java), then type recovery for the ECMAScript family and Python | it is the last family and the only one that needs a project scope, so it is ordered last — but it is ported, not left behind (`15-requirements-audit.md`) |

So there is **no residue**. The phase order leaves the engine owning `calls` for progressively fewer
languages, and the last phase is a retirement gate with five simultaneous measured conditions rather
than an open question (`15-requirements-audit.md`). Until that gate, `calls` for units no precise
indexer covers is still the engine's, which is the role the product already treats as a fallback
(`00-synthesis.md` §3: "`calls` as a fallback where no SCIP profile applies").

## Dependencies this adds

**None for the shared core.** The dominator core is written in the repository rather than imported
(`18-algorithms-dominance-and-dataflow.md` §1), so the analysis adds no module to `go.mod`. A Go-family
compiler-precision call graph would optionally add `golang.org/x/tools` (BSD-3, `go/ssa` +
`go/callgraph`); the highest version readable on this host is v0.49.0. The CST layer, cgo and the nine
grammar registrations (from eight pinned grammar modules) are already pinned.

## What it deletes

`internal/provider/dependence/neo4jcsv` — 4,874 lines of package source (3,446 non-test + 1,428 test),
of which **339 relocate rather than die** (the fact-key comparator the oracle is built on), for a net
credit of **4,535** (`15-requirements-audit.md`) — plus the staging database, its 256 MiB cache, its scratch pool, its
exclusive lock, its retirement rule, the paced reclamation of its freed gigabytes, the six engine
failure classes reverse-engineered from stderr, the per-family memory allowances, the heap-cap
sizing, the OOM retry and the subdivision path (`04-requirements-and-engine-cost.md`). That is real
credit and it belongs in the effort table.
