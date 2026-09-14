# Final recommendation: deep analysis architecture for codectx

Date 2026-09-13. Inputs: reports 01–06 (Opus 5 research, ~200 cited URLs) and 07–08 (my own
runs of Joern 4.0.627 and all six SCIP indexers on this machine). Where a report and a real run
disagreed, the run wins and the correction is recorded in 07/08.

## 0. Second-round verification (reviewer's eight points)

All answered by experiment on this machine; details in `09-scaling-governance-empirical.md`.

| Point | Result |
|---|---|
| Memory envelope on large repos | The multi-GB RSS was JVM default sizing. A 500 MB heap cap produced a byte-identical graph for 239k Go lines (1.0 GB RSS vs 7.2 GB); a 2 GB cap handled 506k Python lines; a too-small cap fails closed with no output. Governor sizes the cap per unit (~2–4 MB per 1k LOC), retries once, never emits partial facts. |
| Call-site join design | Confirmed on 8,325 Go call sites: 97.6% of method/package calls and every bare user-function call resolve at the call-site range; every miss is a builtin, a conversion, a build-tag-excluded file, or a call through a value. Call site and symbol stay separate nodes; the `callsite:` alias joins them and both evidence rows survive. Nothing Joern's CALL edge carries is lost. |
| reads / writes | Produced by the dependence provider from assignment operators: 5,542 assignments on codectx, 3,660 with a resolved written target. Neither SCIP (no write role anywhere) nor syntax alone can do it. |
| Process-tree governance | Heap cap via the frontend's environment; per-unit sizing; `max_concurrent_heavy_analyzers`; `failed: memory` with observed peak; no capability reduction at any cap. |
| Backend provenance | `provider_version` carries the engine payload digest; `Detection.ObservedVersion` reports engine name/version/digest to status and doctor; docs map digests to releases. |
| First-query-only vs background | Changed: `auto` now means low-priority background units after the base index activates, queried units promoted first, typed `pending`. Base readiness is untouched. |
| Cache key closure | Source hashes + manifests/lockfiles + frontend argv (excludes, definition cap, include paths) + engine digest + runtime digest. |
| Long-term backend | Joern remains viable once capped: 239k Go lines in 12 s / 1 GB, 506k Python in 37 s / 3.9 GB. One real engine bug (jssrc2cpg crash on a code shape) was hit on a 1.0M-line Meteor app and on its 324k-line client; per-unit runs contain it. No alternative covers nine languages with control and data dependence: Infer is compositional but C/Java/ObjC and emits no dependence graph; CodeQL is heavier; Fraunhofer cpg has the same JVM profile. Revisit only if the engine bug rate in the CI matrix stays high. |

## 0a. Third-round verification (reviewer's twelve points)
All by experiment; details and raw logs in `10-round3-empirical.md`.

| Point | Result |
|---|---|
| Total RAM of the process tree | Sampled every 0.5 s and summed over descendants. Tree exceeds the largest process by 30–130 MB (Go), ~250 MB (Rust helper). Go 239k LOC full pipeline: 1.0 GB tree. |
| Export memory | Own reservation; capped and lossless (postgres export default 6.5 GB → 2 GB cap 2.8 GB, identical; 1 GB cap fails closed). |
| Python cap | 506k LOC needs 2 GB heap (3.9 GB RSS); 1.05M LOC single package fails at 4 GB but completes uncapped (18.4 GB) → run it whole with the machine-derived allocation; per-package units (100% methods/CDG/dataflow, 79% internal calls) are the natural unit shape, never a memory fallback. |
| C/C++ scale | git 443k LOC: 2 GB cap 3.3 GB RSS, identical. postgres 1.8M LOC: 4 GB heap (6.6 GB RSS) ok, 2 GB fails closed. Per-frontend RSS allowance: C 2.6 GB, Python 1.9 GB, Go/TS/Java ≤0.5 GB. |
| Java/Rust/TS caps | spring 1.5M LOC Java: 6 GB cap ok (3.6× slower, near live set). TS 81k LOC: 1 GB cap 1.3 GB RSS. Rust ripgrep 56k LOC: 2 GB cap 1.8 GB RSS, workspace root is one unit. |
| Per-language callsite join | 100% at exact range on six-language fixtures with the production `.scm` patterns. |
| SCIP Java references | Present (roles omitted when 0 = reference). Only WriteAccess is absent, everywhere. |
| reads/writes constructs | Uniform operator target algebra across six frontends (identifier/fieldAccess/indexAccess/indirection/destructuring temps); four gaps documented. |
| Subdivision parity | TS: CDG/dataflow 99.7–99.9%, resolved calls 46% → never split a tsconfig project. Python per package: 100% methods/CDG/dataflow, 79% resolved calls, cross-package calls alias by full name. |
| Synthesis contradictions | Fixed: `auto` = background after base activation everywhere. |
| Generation visibility | §6 below: sealed unit → new generation reusing active units → atomic activation. |
| Definition-cap retry | Uncapped rerun: `--max-num-def 40000` costs +23% time, +3% memory, zero skips, +1% dataflow edges, all else identical → pinned argv uses 40000 from the start; residual skips are `partial` with method names, no retry. |
| New findings | Go frontend ignores go.work (planner walks go.mod); kubectl module crashes deterministically (`failed: engine`); tokio empty graph with exit 0 (helper panic; must be detected). |

## 1. Why peers feel instant and Joern does not

"Instant" tools skip semantic resolution or let a server precompute it. Of 12 AI-assistant
indexers only Sourcegraph resolves symbols across files, in CI. None compute dataflow or control
dependence. GitHub code-graph tools resolve calls by name ladders with ~80% coverage. Joern runs
a real compiler front end per language, holds the whole program in JVM heap (no spill to disk
since 4.0), runs five overlay passes, has no incremental mode, and we then serialize the graph to
CSV. Measured on codectx (37,590 Go lines): scip-go 0.65 s / 109 MB / 6.9 MB index; Joern parse
4.7 s / 2.2 GB / 4.9 MB cpg, export 2.0 s / 725 MB / 49 MB CSV. With the heap capped, the same corpus needs about 1 GB.

## 2. Verdicts on the reviewer's nine points

| # | Point | Verdict | Evidence |
|---|---|---|---|
| 1 | SCIP alone cannot produce call edges | **Confirmed for all six indexers.** Call-site and function-value references carry identical roles. | 08 |
| 2 | Reimplementing control dependence on tree-sitter | **Do not do it now.** Bounded algorithm (~1.2–1.8k LOC core) but 2.5–4.5k LOC of differential tests, a precision downgrade to `syntax`, and Joern 4.0.627 covers all nine languages (Rust via rust2cpg, needs cargo). | 05, 07 |
| 3 | "Shard per package" | **Shard by the frontend's native unit; never shard C/C++.** Cross-unit calls survive as `IS_EXTERNAL` stubs with full names (verified). Loss: dynamic-dispatch targets across shards; C/C++ include closure. | 06, 07 |
| 4 | `--max-num-def` | **Never lower by default.** Exceeding it zeroes a method's REACHING_DEF with only a stderr WARN; our provider currently discards stderr. Capture the count and report `data_flows_to = partial`. | 06, 07 |
| 5 | Numeric confidence | **Rejected.** Keep factual metadata: precision, provider, resolution strategy, candidate count, ambiguity (`may_refer_to`), generation freshness. | model already has all but candidate count |
| 6 | Content-hash caching | **Already correct in the design.** File-local units key on blob hash + grammar version; semantic units key on the input closure (UnitID). No change. | Sections 9.1, 13.1 |
| 7 | Joern fully lazy | **Yes, extraction-lazy.** Overlays persist in cpg.bin; cache cpg per (unit, input hash, engine version); parse in the background after the base generation activates (`auto`), queried units first; never on the base-readiness path. `--nooverlays`/`--overlaysonly` two-phase is broken at 4.0.x. | 06, 07 |
| 8 | Bounded extraction vs full CSV | **Keep one `--repr=all --format=neo4jcsv` export.** `joern-slice` cannot emit CDG; DOT views are display-filtered projections. Measured export cost is acceptable (2 s, 49 MB for 37k lines). | 06, 07 |
| 9 | Competitor ratios as requirements | **Agreed.** Our own harness (Task 21) publishes wall, process-tree RSS, disk, and ladder-vs-SCIP precision. | 03 |

## 3. Final architecture

**Tier 1, base (always, seconds).** Tree-sitter structural facts keyed by blob hash. Call sites
are syntax facts: the tree-sitter provider publishes each call site with an alias
`callsite:<path>:<start>-<end>` in scope `file:<path>` and a `may_refer_to`/unresolved callee
node carrying `resolution` strategy and `candidates` count as factual attributes.

**Tier 2, precise (managed SCIP, seconds to minutes, per package).** The SCIP importer emits the
same `callsite:` alias for every reference occurrence whose range equals a tree-sitter call-site
range in the same file version. The reconciler merges the two nodes, so the `calls` edge acquires
a compiler-precision target with both evidence rows retained. Reads/writes cannot come from SCIP
role bits (no indexer sets WriteAccess); they stay a dependence-tier fact.

**Tier 3, dependence (managed engine, background after base, per-unit, capped, cached).** Provider id `dependence`, facts
`control_depends_on`, `data_flows_to`, and `calls` as a fallback where no SCIP profile applies.
The engine is Joern 4.0.627 behind `internal/provider/dependence/joern`; the name Joern appears in
the repo, the lock, and docs only. Unit = frontend-native project (Go module, Maven/Gradle module,
package.json or tsconfig project, Python package, Cargo package); C/C++ analyzed as one unit with
real include paths. One `joern-parse --language <l>` per unit, one `--repr=all --format=neo4jcsv`
export. cpg.bin cached per (unit, input hash, engine digest). Default `enabled = "auto"`: nothing runs before the base generation activates; units then run as
low-priority background work under the governor, queried units first, with a typed `pending` answer
until they seal. Heap cap per unit from its size; fail closed on OOM. `true` runs it in
every index. `--max-num-def` stays at the engine default; WARN count → `partial`.

**Evidence.** Precision vocabulary unchanged. Add `candidates` (int) and `resolution` (enum:
exact, import, same_module, unique_name, ambiguous, unresolved) as node attributes on tree-sitter
callee nodes. Query results expose precision, provider, generation, and these attributes; no score.

## 4. Required plan changes (in place)

1. **Task 11 fix round (before Task 12 dispatch):** rename provider/package to `dependence` with
   a `joern` backend; always pass `--language`; one unit per frontend-native project, C/C++
   unsharded; single export, delete GraphML path; capture stderr and report `partial` with skip
   count; correct `data_flows_to` detail (not strictly intraprocedural: closures/globals cross
   methods); do not drop `IS_EXTERNAL` methods that carry a snapshot filename; Rust unit requires
   `Cargo.toml` and cargo on the engine PATH allowlist; neutral evidence details (`cdg`,
   `reaching_def`, `call`); provider_version carries the engine digest, not the engine name.
2. **Task 8/9 fix round:** `callsite:` alias convention in both providers; SCIP importer emits it
   for occurrences matching a call-site range; tree-sitter adds `resolution` and `candidates`.
3. **Task 12:** dependence units are low-priority background work after base activation (`auto`), blocking with `enabled=true`, off with `false`; each sealed unit is published as a new generation that reuses the active base units (see §6); cpg cache in the
   data dir with retention; typed `pending` capability state.
4. **Task 14/15:** graph and context surfaces return provenance fields, never a confidence number.
5. **Task 22:** lock entry `joern` maps to capability `dependence` in `tools status` output; engine
   prerequisites (cargo, maven, node_modules) → `dependencies_unresolved`.
6. **Task 21:** publish tier-1/2/3 wall, RSS, disk, and tree-sitter-vs-SCIP call precision.
7. **Spec:** Section 11.6 becomes "Dependence Provider"; config `[providers.dependence]`;
   Section 18/19 wording drops the engine name.

## 5. Trade-offs accepted
- Two `calls` sources at two precisions until SCIP covers a repo; consumers see which via evidence.
- The engine stays the only source of control and data dependence; its cost runs in the background after base activation, capped by the governor, and is skipped entirely with `enabled=false`.
- Renaming hides the engine from users but the lock and docs still name it for licensing.
- C/C++ dependence remains whole-project, the one place sharding cannot help.

## 6. Generation visibility for late-sealing dependence units
A generation is immutable once active; dependence units seal minutes later. The coordinator
therefore never mutates the active generation. When a dependence unit seals it creates a new
generation whose unit set is the active generation's units plus the new dependence unit, validates it,
and flips the active pointer atomically (the same Section 13 path a refresh uses). Readers pinned
to the older generation keep a consistent view; new requests see the added facts. Consecutive
seals coalesce: units that seal within one scheduler tick publish as one generation. Retention
treats these generations like any other (Section 20). Facts from a unit are therefore visible
either completely or not at all, never partially, and the `pending` capability answer flips to a
real answer exactly at activation.

## 7. Round-3 evidence
See `10-round3-empirical.md` for the tree-summed memory tables (parse and export), per-language
caps on C, Rust, TypeScript, Java and large Python, the Kubernetes go.work finding, the six-language
call-site join, the reads/writes lowering algebra, subdivision parity and the definition-cap retry.

## 8. Direction ruling (2026-09-13, user-adopted reviewer directive)
Joern-backed `dependence` is the MVP backend. No default memory ceiling: estimates schedule and
serialize work (`max_concurrent_heavy_analyzers = 1` by default, co-schedule only when summed
reservations fit); only an explicit user limit rejects work up front; one OOM retry at the
machine-derived allocation, only when it exceeds the failed cap. Subdivision is the last-resort
recovery from a reproducible engine crash (confirmed by one rerun with the frontend's fixed,
currently empty, semantics-neutral option allowlist), never for memory, published per capability as
`partial: subdivided` with failed unit, backend failure and affected capabilities. No lowered
analysis limits or omitted fact families. Every advertised language (nine languages, six
frontends; C++ and TSX still to be exercised) runs end to end in Task 22 with real tools. Benchmark
corpora are pinned by commit in Task 21 as the differential oracle for any future native engine.
