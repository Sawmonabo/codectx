# Round 3 empirical results (2026-09-13, linux/amd64, 16 cores, 47 GiB, Joern 4.0.627, JDK 21)

Raw logs, scripts and fixtures: `raw/`. Every number is from a real run on this machine.
Memory is reported two ways: `largest` = peak RSS of the biggest single process (`/usr/bin/time %M`),
and `tree` = peak of the simultaneous sum of RSS over the whole descendant tree, sampled every
0.5 s (`raw/treemem.sh`). The two differ by the orchestrating JVM and helper processes.

## 1. Tree-summed memory: parse and export, default vs capped (x/tools, 239k Go LOC)

| Step | Heap cap | wall | largest RSS | tree RSS | procs at peak | output |
|---|---|---|---|---|---|---|
| parse | default | 16.6 s | 4.79 GB | 4.81 GB | 4 | cpg 14,308 KB |
| parse | 1.5 GB | 23.7 s | 1.81 GB | 1.94 GB | 4 | identical |
| parse | 500 MB | 23.2 s | 0.98 GB | 1.02 GB | 4 | identical |
| export | default | 5.1 s | 1.20 GB | 1.20 GB | 2 | CSV 145,476 KB |
| export | 1 GB | 4.5 s | 0.73 GB | 0.73 GB | 2 | identical |
| export | 512 MB | 4.5 s | 0.53 GB | 0.53 GB | 2 | identical |

The tree sum exceeds the largest process by 30–130 MB (the joern-parse orchestrator JVM plus
goastgen). `%M` was therefore a fair proxy in earlier rounds, but the tree figure is what the
governor accounts. Full parse→export pipeline for 239k Go lines: **about 1.0 GB tree peak**
with a 500 MB parse cap and 512 MB export cap, byte-identical facts.

## 2. Tree-sitter call site + SCIP occurrence join, all six languages
Using the production `.scm` call patterns (`@call.name`) against the real SCIP indexes from
report 08 (`raw/callsite-join.py`):

| Language | indexer | call sites | resolved at exact range | notes |
|---|---|---|---|---|
| Go | scip-go 0.2.7 | 2 | 2 | `f(2)` → `local 1` (function value) |
| Python | scip-python 0.6.6 | 2 | 2 | same |
| TypeScript | scip-typescript 0.4.0 | 2 | 2 | uses the shared `ecmascript.scm` patterns |
| Java | scip-java 0.13.1 | 2 | 2 | `f.applyAsInt(2)` → `IntUnaryOperator#applyAsInt().` (interface method, correct) |
| Rust | rust-analyzer 1.98 | 2 | 2 | |
| C | scip-clang 0.4.0 | 2 | 2 | |

Java correction to report 08's table: reference occurrences are present (roles field omitted
because the value is 0 = reference); only WriteAccess is absent everywhere. Column encodings
agreed for ASCII fixtures; the production join must convert through the shared UTF-8/UTF-16
position helper (Section 9.3) before comparing, since scip-typescript and scip-java emit UTF-16
columns.

## 3. Reads/writes: what the engine lowers every write form into (`raw/rw-fixtures`)
All six frontends lower writes to operator calls: `<operator>.assignment`, `assignmentPlus`,
`assignmentMinus`, `postIncrement`, `preDecrement`/`preIncrement`. The written operand is argument 1
and has one of these shapes:

| Shape | Example | Target resolution |
|---|---|---|
| identifier with REF | `x += 2`, `g = x` (C/JS/Py) | direct: REF edge to LOCAL/PARAM/global |
| `fieldAccess(base, field)` | `s.f = 3` (all), `(*self).f` (Rust) | base identifier resolves; field via FIELD_IDENTIFIER + base type's MEMBER when typed |
| `indexAccess(container, idx)` | `s.arr[i] = 4`, `d["k"] = 6` | write to the container expression's base |
| `indirection(ptr)` | `*p = 5` (C/Go/Rust) | write through pointer `p` |
| destructuring | `a, b = b, a` (Py/JS/Rust), `let [a,b] = …` | lowered to a temp plus one assignment per element (Python `tmp0`, JS `_tmp_1`, Rust `<tmp>0`) |
| chained | `y = x = 7` | two assignments, both targets resolve |

Gaps found: Go `a, b = b, a` lowers only the first target (gosrc2cpg); Rust `(a, b) = (b, a)` keeps
a `tupleLiteral` target (needs unpacking); Java static field `W.g = x` and Go package global
`G = x` appear as `fieldAccess` on a type/package base rather than a REF'd identifier; Rust
`static mut G` is unresolved. Rule for the provider: `writes` = method → declaration for shapes
whose innermost identifier has a REF (precise) or whose field resolves to a MEMBER (precise);
otherwise a `may_refer_to` with the syntactic name. Compound ops count as both read and write.

## 4. Large repositories, per language, default vs capped (tree-summed RSS)
Repos: git (C, 443k LOC), postgres (C, 1.8M LOC), spring-framework (Java, 1.5M LOC),
r3/app/both (TypeScript, 81k LOC), tokio (Rust, 183k LOC), Kubernetes staging modules (Go),
m32rimm (Python, 1.05M LOC, report 09), x/tools (Go, 239k LOC, §1).

| Repo (lang, LOC) | parse cap | parse wall | parse tree RSS | export cap | export wall | export tree RSS | CSV | facts identical? |
|---|---|---|---|---|---|---|---|---|
| git (C, 443k) | default | 16.3 s | 5.57 GB | default | 17.3 s | 5.48 GB | 650 MB | baseline (4 methods over def cap) |
| git | 2 GB | 17.1 s | 3.30 GB | 1 GB | 16.8 s | 1.32 GB | 650 MB | yes, byte-equal counts |
| r3 both (TS, 81k) | default | 17.9 s | 4.76 GB | default | 5.6 s | 1.04 GB | 193 MB | baseline |
| r3 both | 1 GB | 17.8 s | 1.31 GB | 512 MB | 5.8 s | 0.49 GB | 193 MB | yes |
| spring (Java, 1.5M) | default | 37.6 s | 7.91 GB | default | 17.9 s | 3.03 GB | 709 MB | baseline |
| spring | 6 GB | 135.5 s | 6.37 GB | 2 GB | 20.3 s | 1.48 GB | 709 MB | CALL/METHOD equal; CDG −4, REACHING_DEF −6 (engine run-to-run variance, 0.001%) |
| postgres (C, 1.8M) | default | 45.8 s | 10.22 GB | default | 49.7 s | 6.51 GB | 2.02 GB | baseline (9 methods over def cap) |
| postgres | 8 GB | 45.7 s | 9.77 GB | 2 GB | see §4a | | | |
| m32rimm (Py, 1.05M) | default | 105 s | 18.4 GB | default | 94.7 s | 9.5 GB | 4.95 GB | baseline (2 over def cap) |
| m32rimm | 4 GB | fails closed (OOM) | | | | | | |

Observations. (1) Caps are lossless everywhere they succeed. (2) A heap cap is not an RSS cap:
JVM RSS runs 0.3–2 GB above `-Xmx` (metaspace, native parser memory, helper processes); the
governor must reserve cap × ~1.3 plus the helper allowance. (3) A cap close to the live set costs
time instead of memory: spring at 6 GB ran 3.6× slower than default because the collector thrashed.
The governor should therefore size from bytes with headroom rather than hunt for the smallest
passing cap. (4) Export is cheaper than parse but scales with the graph, not the source: 2 GB CSV
for postgres, 4.95 GB for m32rimm; export needs its own reservation and the CSV is deleted after import.

## 4a. The two units that cannot be shrunk (C/C++ and giant Python)
Postgres and m32rimm are single frontend-native units (C is header-coupled; m32rimm is one
Python package tree). See the pgcap results appended below for the postgres 4 GB / 2 GB attempts.
Under the adopted direction (`00-synthesis.md` §8) these units run whole at the machine-derived
allocation; on a machine that cannot hold them the answer is `failed: memory` with the observed
requirement, never a silent partial graph and never a memory-motivated split.

## 5. Kubernetes (Go, go.work multi-module, staging modules)
Whole-repo parse at default and 8 GB both covered only 36 files (the frontend reads go.work's
root module only); the unit planner must walk `go.mod` files itself. Per-module at 4 GB cap
(`largest` RSS; tree adds ≤130 MB for Go):

| module | LOC | wall | largest RSS | result |
|---|---|---|---|---|
| api | 438,877 | 23.9 s | 4.90 GB | ok |
| apiextensions-apiserver | 120,558 | 8.2 s | 3.93 GB | ok |
| apimachinery | 143,404 | 8.3 s | 3.38 GB | ok |
| apiserver | 304,975 | 11.4 s | 3.38 GB | ok |
| client-go | 344,975 | 9.9 s | 2.76 GB | ok |
| code-generator | 133,599 | 7.9 s | 2.51 GB | ok |
| kubectl | 136,949 | 5.9 s | 3.34 GB | **engine crash** `CfgCreationPass` `NoSuchElementException: next on empty iterator`; reproduced at default heap → deterministic, not memory |

scip-go on the whole Kubernetes workspace (for comparison): 151.8 s, 4.23 GB RSS, 317 MB index.
Note the original run failed because `GOFLAGS=-mod=mod` is rejected in workspace mode; the
profile must not set that flag when a `go.work` is present.

## 6. Engine failure modes the provider must classify (all observed)
| Mode | Example | Signal | Provider state |
|---|---|---|---|
| Heap exhaustion | m32rimm at 4 GB, redglass at 1 GB | non-zero exit, `OutOfMemoryError` on stderr, no cpg | `failed: memory` (retry once at the machine-derived allocation when it exceeds the failed cap) |
| Deterministic pass crash | kubectl module, r3 client whole | non-zero exit, `Pass … failed` + exception on stderr, no cpg | `failed: engine` with pass name; no retry; siblings unaffected |
| Helper crash with **zero exit** | tokio (Rust): bundled rust-analyzer panics `should have ExpressionStore::expr_only`, then joern-parse prints "Successfully wrote graph" | exit 0, cpg 16–20 KB, 1 FILE node, stderr contains `Process exited with code 101` | must be detected: FILE count 0 for a non-empty unit, or the `Process exited with code` line → `failed: engine`. Exit code alone is a lie here. |
| Definition cap exceeded | git 4, postgres 9, both 4, m32 2 methods | `Skipping.` WARN lines | `partial` with `skipped_methods` (retry policy §8) |
| Wrong unit shape | bare `.rs` dir without `Cargo.toml`, go.work root | exit 0, near-empty graph | planner rule: Cargo project root (workspaces parse whole: ripgrep 11 crates → 103 files, 4,616 methods in 10.7 s / 1.77 GB), every go.mod; never accept an empty graph silently |

## 7. Rust at scale (ripgrep 56k LOC, 11 crates; tokio 183k LOC)
| Unit | wall | largest RSS (2 GB cap) | FILE | METHOD | CALL | CDG | REACHING_DEF |
|---|---|---|---|---|---|---|---|
| ripgrep workspace root | 10.7 s | 1.77 GB | 103 | 4,616 | 56,692 | 52,223 | 257,818 |
| crates/core | 7.4 s | 0.92 GB | 23 | 1,696 | 15,130 | 12,120 | 73,605 |
| crates/ignore | 10.8 s | 0.88 GB | 13 | 856 | 12,018 | 8,905 | 41,894 |
| crates/cli | 6.0 s | 0.82 GB | 9 | 290 | 1,635 | 1,541 | 8,428 |
| other crates | 5–12 s | 0.81–0.83 GB | 3–6 | 52–397 | | | |

Workspace-root parsing works and is the right unit for Rust (the Rust helper runs `cargo metadata`
against the workspace, loads the sysroot from rustup, and type-resolves across member crates).
Per-crate units cost about 0.8 GB each of fixed Rust-analyzer overhead, so a workspace is one unit.
tokio (both root and `tokio/tokio`) fails inside the bundled Rust analyzer 0.0.332 with
`should have ExpressionStore::expr_only`; the orchestrator still exits 0 and writes a 16 KB graph.
This is the crate-specific engine bug in §6, and the detection rule there is mandatory.

## 4b. Postgres (C, 1.8M LOC, one unit) cap sweep — the C/C++ floor
| parse cap | parse wall | largest RSS | result | export cap | export result |
|---|---|---|---|---|---|
| default | 45.8 s | 10.22 GB | ok | default | ok, 6.51 GB |
| 8 GB | 45.7 s | 10.05 GB | ok, identical facts | 2 GB | ok, 2.78 GB, identical facts |
| 4 GB | 61.3 s | 6.62 GB | ok, identical cpg size | 1 GB | **OOM, fails closed** (2.0 GB RSS, no CSV) |
| 2 GB | 30.6 s | 2.52 GB | **OOM, fails closed, no cpg** | | |

The C frontend keeps about 2.6 GB outside the Java heap at the 4 GB cap (native parser memory),
so RSS overhead is frontend-specific, not a fixed ratio: Go/TS/Java ran 0.3–0.4 GB above the cap,
C 2.6 GB, Python 1.9 GB (redglass 2 GB cap → 3.9 GB RSS). The governor keys the overhead
allowance per frontend from these measurements (table in §9), and re-learns it from observed peaks.
A 1.8M-line C repo therefore needs ~6.6 GB for parse and ~2.8 GB for export as a single unit;
on a machine with less free memory than that it is `failed: memory` with the observed requirement after the machine-derived retry. Whole-repo C is the one unit the
planner cannot split today; per-directory C units are possible in principle (each translation
unit is parsed independently against its include paths, only cross-directory call linking would
move to the alias join) and are left as the documented follow-up if a real user hits this.

## 8. Subdivision parity: what splitting a project into smaller units costs
Method: parse the whole project as one unit, then each top-level subdirectory as its own unit,
export both, and compare fact counts (`raw/parity.txt`; operators excluded from call counts).

**TypeScript, r3/app/both (one tsconfig project, 4 subdirs).**
| | methods (internal) | calls resolved to internal methods | CDG edges | REACHING_DEF edges | total CALL edges |
|---|---|---|---|---|---|
| whole | 4,331 | 11,148 | 81,040 | 1,083,532 | 176,698 |
| sum of 4 subdir units | 4,322 | 5,113 (46%) | 80,770 (99.7%) | 1,082,613 (99.9%) | 147,716 (84%) |

Control and data dependence are intra-procedural and survive splitting intact. Call resolution
does not: more than half of the resolved calls cross subdirectory boundaries and become
unresolved when the importing file is parsed without its target, and 16% of call edges vanish
entirely (the TypeScript frontend needs the whole `tsconfig` program to type receivers).
**Ruling: a TypeScript/JavaScript unit is the `tsconfig`/`package.json` project, never a
subdirectory.** Splitting is only the crash fallback (r3 client), and a fallback unit is
published per capability as `partial: subdivided` (control/data dependence near-complete, engine `calls` degraded; consumers use the syntax-plus-SCIP `calls` path) so nobody mistakes it for a full-unit result. Subdivision is never used for memory.

**Python, redglass/packages (7 packages).**
| | methods (internal) | calls resolved to internal methods | CDG | REACHING_DEF |
|---|---|---|---|---|
| whole | 26,451 | 44,887 | 276,219 | 2,947,436 |
| sum of 7 package units | 26,451 (100%) | 35,428 (79%) | 276,219 (100%) | 2,950,679 (100.1%) |

Per-package Python keeps every method and every dependence edge; the 21% of internal calls that
cross packages resolve to external stubs named by module path, which the canonical layer aliases
by full name (Section 11.6). The whole-tree run also emitted 2.41M external call edges against
1.35M for the units: the whole-program type recovery fans calls out to many candidate targets,
so "more edges" there is lower precision, not more knowledge. **Ruling: Python units are packages
(directory with `__init__.py`/`pyproject.toml`), exactly as the plan says; m32rimm (1.05M LOC,
one flat package tree) is run whole at the machine-derived allocation; it completed uncapped at 18.4 GB. Splitting for memory is not done.**

## 9. Definition-cap retry (`--max-num-def`)
m32rimm at the default cap skipped 2 methods (`partial`). Both are module bodies of generated
constant tables:
```
fisio/fisio/common/test_utils/constants.py:<module> has more than 4000 definitions
utils/customer/AEP/tenable/test_residual_risk_asset_csv_dump.py:<module> has more than 4000 definitions
```
Retrying with `--max-num-def 40000` under a 6 GB cap ran 100 s and died of OOM at 8.1 GB RSS
(tree). Data-flow over a 4,000-assignment constant table is quadratic and worthless.
That run was under an artificial 6 GB cap on a unit that needs ~18 GB uncapped, so it is not
conclusive on its own. The uncapped rerun is recorded in §9a below and decides the policy.
`partial` carries the skipped method names (parsed from the WARN lines) so a consumer can see
exactly which bodies lack data dependence; `--max-num-def` stays at the engine default and is
part of the cache key.

## 9a. Definition cap, uncapped rerun (decides the policy)
Same 1.05M-line Python tree, no heap cap, machine has 47 GB (`raw/retry.txt`):

| `--max-num-def` | parse wall | CPU | parse tree RSS | skipped methods | REACHING_DEF | CDG | CALL | export |
|---|---|---|---|---|---|---|---|---|
| 4000 (engine default) | 86.2 s | 771 s | 18.3 GB | 2 | 13,811,726 | 1,307,034 | 44,695,131 | 95.3 s, 7.4 GB |
| 40000 | 106.2 s | 867 s | 18.9 GB | 0 | 13,953,749 (+1.0%) | identical | identical | 96.5 s, 7.2 GB |

With real memory available the higher cap costs 23% more parse time and 3% more memory, removes
every skip, and changes nothing outside the two previously skipped bodies (CDG and CALL counts
are identical). The earlier "prohibitive" result was purely the artificial 6 GB cap.
**Ruling: the pinned parse argv uses `--max-num-def 40000` from the start** (no second parse, so no
doubled cost), the value is part of the cache key, and a body that still exceeds it is published
`partial` with the skipped method names. No further retry.

## 9b. Engine run-to-run variance (the band every parity claim is bounded by)
The engine is not run-to-run deterministic. Two runs of the same pinned argv over the same
unmodified tree (this repository, 161 files, Go frontend) produced:

| run | nodes | relations | aliases | dropped methods | external methods |
|---|---|---|---|---|---|
| 1 | 13,675 | 52,310 | 16,922 | 3,983 | 1,320 |
| 2 | 13,677 | 52,311 | 16,926 | 3,990 | 1,322 |

About 0.01%, and the same order as the CDG −4 / REACHING_DEF −6 seen on spring in §4. Recorded
here so that no later reviewer re-litigates a non-zero diff between two engine runs as a defect,
and so that every parity claim in §4, §8 and §9a is read as "no systematic loss and no fact class
missing, within this band" rather than as equality.

## 10. Per-frontend memory model for the governor (measured)
| frontend | RSS above heap cap | notes |
|---|---|---|
| Go (gosrc2cpg + goastgen) | 0.3–0.5 GB tree | helper is a Go binary, small |
| TypeScript/JS (jssrc2cpg + astgen) | 0.3 GB | |
| Java (javasrc2cpg) | 0.1–0.4 GB | near-cap runs thrash: 6 GB cap 3.6× slower than default |
| Rust (rust2cpg + rust_ast_gen) | ~0.25 GB tree; helper ≈ 0.8 GB fixed | helper is a Rust binary outside the JVM |
| Python (pysrc2cpg) | 1.9 GB | 506k LOC → 2 GB cap ok / 1 GB fails; 1.05M LOC → 4 GB fails |
| C/C++ (c2cpg) | 2.6 GB | native parser memory |
| export (joern-export) | 0.3–0.8 GB | scales with graph; CSV = 5–10× cpg |

Reservation = heap cap + frontend allowance + helper allowance; the heap cap is chosen from unit
bytes with headroom (sizing against the smallest passing cap costs time, see spring). Reservations
schedule and serialize units; only an explicit user `unit_memory_ceiling_bytes` rejects a unit before
it runs (ruling in `00-synthesis.md` §8).

## 11. Cleanup
After this report was written: clones (kubernetes, git, postgres, spring-framework, tokio,
ripgrep), the x/tools and m32rimm copies, all `.bin`/`.exp`/`.scip` artifacts and the venvs were
deleted from the scratchpad. Installed tools stay (report 08 list) for the implementation wave.
