# Native-engine research, raw evidence — corrections to the cited research, and two product defects found on the way

Every correction below is a figure this lane re-measured or re-verified, against a claim that is
currently written down somewhere in the repository. Each names what to change.

## Corrections to `docs/research/05-cfg-cdg-from-treesitter.md`

| # | Claim as written | Correct at v4.0.627 | Source |
|---|---|---|---|
| 1 | §1.4 and §6: **"Joern has no Rust frontend"**; `control_depends_on` for Rust "is zero today"; building the Rust mapping is therefore "a requirement, not a choice" | **False.** `joern-cli/frontends/rust2cpg/` exists: 2,945 main / 9,704 test Scala lines. Rust is **not** skipped in `ControlFlow.scala:17-24` (only GHIDRA and LLVM are), and every ControlStructure type rust2cpg emits is a strict subset of `CfgCreator`'s 14 dispatches, so nothing degrades to `Cfg.empty`. The engine publishes **non-empty** Rust CDG and REACHING_DEF. | `12-source-anatomy-v4.0.627.md` §A6 |
| 2 | §1.1 table: `CfgCreator.scala` = **649** lines | **773** (+124, +19.1%) | `x2cpg/.../cfgcreation/CfgCreator.scala` |
| 3 | §1.1 table: `CdgPass.scala` = **62** lines | **68** (+6, +9.7%) | `x2cpg/.../codepencegraph/CdgPass.scala` |
| 4 | §1.1 table: `Cfg.scala` 197, `CfgCreationPass.scala` 27, `CfgDominator.scala` 90 | **unchanged** — confirmed at v4.0.627 across 527 patch releases | same files |
| 5 | §3: `stack-graphs` treated as a live-but-inapplicable project | **Archived 2025-09-09**; crate 0.14.1 last published 2024-12-13; no `cdylib`/`staticlib` in its manifest, so there is no C ABI to cgo against. The verdict ("not applicable — no CFG") is unchanged; the reason is stronger. | `09-native-building-blocks.md` |
| 6 | §4's per-language estimate anchors the Joern-side upper bound on the v4.0.100 statement creators | Those creators **grew**, unevenly: c2cpg 321→**634**, javasrc2cpg ≈260→**998**, pysrc2cpg 2,349→**3,172**; jssrc2cpg 916→**816** and gosrc2cpg 273→**266** shrank. The Fraunhofer-side anchor (125-line Go delta) did not move. **That spread is the uncertainty** and the estimate must say which anchor each range rests on. | `12-source-anatomy-v4.0.627.md` §A7 |

## Correction to this lane's own brief (and to `docs/providers-dependence.md`'s wording)

| # | Claim | Correct at v4.0.627 | Source |
|---|---|---|---|
| 7 | "`--repr=all --format=neo4jcsv` is the **only legal** way to get CDG + REACHING_DEF + CALL in CSV" | **False as stated.** `Format.Neo4jCsv` accepts **two** reprs: `All` and `Cpg` (`JoernExport.scala:144-166`); every other repr throws. `--repr=cpg --format=neo4jcsv` is legal — but **unusable**, for three reasons: it emits one output directory per method (`:152-161`); `MethodSubGraph.edges` filters `if nodes.contains(edge.dst)` (`:230`), dropping every inter-procedural CALL edge; and the node set is `method.ast.toSet` (`:177`), so TYPE_DECL and MEMBER never appear. The correct wording is **"the only usable way"**. | `12-source-anatomy-v4.0.627.md` §A5 |
| 8 | This research brief §2: the product imports "`REACHING_DEF` edges **with the variable**" | **The product discards the variable.** The export carries it — every fixture header is `:START_ID,:END_ID,:TYPE,VARIABLE:string` — and `putEdge` deliberately keeps only `(label, src, dst)` (`scratch.go:413-420`; the `edge_in` schema at `:172` has no property column). | `10-reaching-defs-and-readswrites.md` Unit 1 (d) |

## Two product defects surfaced (for the controller's ledger, not for this plan to fix)

| # | Defect | Evidence |
|---|---|---|
| D1 | **`docs/providers-dependence.md:22` lists a `global` evidence detail that no code path emits.** Globals are reported as `reaching_def capture`, because Joern's global edges (`DdgGenerator.scala:190-200`) carry no marker in the export — same `REACHING_DEF` type — so the product infers a capture from `closure_binding <> ''`. Either the table drops `global` or it states the folding. | `grep -rn 'detailGlobal\|"global"' internal/` is empty; `scratch.go:631-636` emits only two details |
| D2 | **The Rust dependence families have a named fidelity gap with no upstream coverage.** `?`, the early-return mechanism of idiomatic Rust, is lowered to a plain CALL with no control structure (`RustVisitor.scala:1355-1359`), so every statement after a `?` is recorded as unconditionally reachable when it is in fact control-dependent on the `?` succeeding. Labeled `break`/`continue` collapse to the innermost loop; `for`/`if let`/`while let` conditions are UNKNOWN, giving CDG no true/false discrimination. rust2cpg ships **zero** CFG/CDG/dataflow tests (all 35 test files are AST-level) against c2cpg's 11 and jssrc2cpg's 9. This is not a mislabel — `internal/model/facts.go:89-91` says `Precision` describes origin, "not guaranteed soundness" — but it is a gap the controller should hold. | `12-source-anatomy-v4.0.627.md` §A6 |

## One correction to a defect in the reference implementation (noted, not actionable here)

`semanticcpg/.../operatorextension/package.scala:19` lists `Operators.postIncrement` **twice**, so
`Operators.postDecrement` (which exists — 15 uses in the checkout) is absent from both
`allAssignmentTypes` and `allArithmeticTypes`. The product's `rw.go:25` lists it explicitly, so
**codectx is more correct than the library here.** Recorded so nobody "fixes" `rw.go` to match.
