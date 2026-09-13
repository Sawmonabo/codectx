# Joern 4.0.627 empirical verification (linux/amd64, JDK 21.0.12, 2026-09-13)

Archive: joern-cli-linux-x86_64.zip from github.com/joernio/joern/releases/tag/v4.0.627
SHA-256: 964655bdffeea5564eb4ed37f58b640c42226b43aae792aeca5f0e02796e120f (1.86 GB)
Install: ~/.local/opt/joern-cli. Fixtures: internal/provider/treesitter/testdata plus
one branching function per language (scratchpad).

## Pinned Task 11 argv is broken
`joern-export cpg.bin --repr=pdg --format=graphml --out X` →
`Exception in thread "main" scala.NotImplementedError: repr=Pdg not yet supported for this format`
Same for `--repr=cdg --format=neo4jcsv`. Only `all` and `cpg` work with neo4jcsv/graphml;
`pdg|cdg|ddg|ast|cfg|cpg14` work only with `--format=dot` (one file per method).

## `--repr=all --format=neo4jcsv` already carries every fact we import
Edge files present: ALIAS_OF ARGUMENT AST BINDS CALL CAPTURE **CDG** CFG CONTAINS DOMINATE
EVAL_TYPE IMPORTS PARAMETER_LINK POST_DOMINATE **REACHING_DEF** REF SOURCE_FILE.
Overlays are applied by joern-parse and persisted in cpg.bin (export took 0.65 s, no
overlay rerun; `--nooverlays` parse then export has no CALL/CDG files).

Branching fixture (if/else + for) per frontend:

| frontend | CONTROL_STRUCTURE | CDG edges | REACHING_DEF edges |
|---|---|---|---|
| golang | 2 | 16 | 105 |
| pythonsrc | 2 | 19 | 140 |
| javasrc | 2 | 16 | 111 |
| jssrc | 2 | 16 | 118 |
| c | 2 | 16 | 105 |

## joern-parse handles ONE language per invocation
Nine-language directory without `--language` → "language: `JSSRC`", only sample.js/.ts/.tsx
parsed. Correct selectors: `c`, `golang`, `pythonsrc`, `javasrc`, `jssrc`, `rust`, `kotlin`…
(`python` maps to a legacy py2cpg.sh that does not exist; `go` fails with None.get).
Consequence: the current provider, which runs joern-parse once on the whole materialization
with no `--language`, analyzes only one language of a polyglot repo. The unit must be
(language, root).

## Per-invocation floor on a one-file fixture (wall, peak RSS)
c 1.6 s 240 MB · golang 1.5 s 233 MB · pythonsrc 1.6 s 249 MB · jssrc 2.0 s 293 MB ·
javasrc 4.9 s 495 MB. CSV export of a ~100 KB cpg.bin is ~5x its size (372–556 KB).

## `--max-num-def` degrades silently
`--max-num-def 1` → stderr `WARN ReachingDefPass Skipping.` (no method name), REACHING_DEF
rows 109 → 16. Default is 4000. Do not lower by default; if ever used, count WARN lines and
report a degraded unit.

## joern-slice (pinned CLI, JSON)
`joern-slice data-flow cpg.bin -o s` → `{"$type":"DataFlowSlice","nodes":[{id,label,code,
name,lineNumber,columnNumber,parentMethod,parentFile,typeFullName}],"edges":[{src,dst,
label:"REACHING_DEF"}]}` — REACHING_DEF only, no CDG, no CALL. Flags: --file-filter,
--method-name-filter, --slice-depth (default 20), --sink-filter.
`joern-slice usages` → per-object usage slices (argToCalls, invokedCalls); not our fact model.
`joern-export --repr=pdg --format=dot` → per-method DOT with CDG+DDG edges, 48 KB for the C
fixture. Neither slice mode gives CDG; the `all` CSV export is the only single artifact that
yields CALL + CDG + REACHING_DEF with node attributes.

## Bundled frontend helpers
gosrc2cpg and jssrc2cpg ship their astgen binaries inside the archive
(frontends/gosrc2cpg/bin/astgen/goastgen-linux, frontends/jssrc2cpg/bin/astgen/*); no
runtime download observed. The archive is platform-specific for that reason.

## Sharding experiment (gosrc2cpg): whole module vs one package
Two-package Go module; `b.Run` calls `a.Helper` twice.
- whole module: METHOD `example.com/w/a.Helper` IS_EXTERNAL=false; 2 CALL edges to it.
- only package b: METHOD `example.com/w/a.Helper` IS_EXTERNAL=**true** (stub), still 2 CALL edges,
  same FULL_NAME.
So for import-resolved static calls the shard keeps the edge and the callee's fully qualified
name; it loses only the callee's body facts (which live in the other shard) and any resolution
that needs the callee's body or type hierarchy (dynamic dispatch, interface methods). The stub
FULL_NAME is an alias key our reconciler can bind to the real declaration from the other shard
or from SCIP.

## Rust frontend exists in 4.0.627 (correction to report 05)
`joern-parse --language rust` runs `frontends/rust2cpg` (bundled `rust_ast_gen-linux`, a
rust-analyzer-based generator). It requires a Cargo project and `cargo`/`rustc` on PATH; a bare
`.rs` directory yields an empty CPG (FILE=<empty>) with `rust_ast_gen::cargo … no projects`.
On the Cargo fixture: 3.7 s, 522 MB RSS, METHOD=11 CONTROL_STRUCTURE=2 CDG=17 REACHING_DEF=156
CALL=19. So all nine codectx languages are covered by Joern (c2cpg for C/C++, jssrc2cpg for
JS/TS/TSX), and report 05's "no Rust frontend" applies to v4.0.100 source, not to the release we
pin. Report 05's fidelity findings about the shared CfgCreator and per-frontend gaps still stand.

## Real-corpus measurement: codectx itself (126 Go files, 37,590 lines), this machine (16 cores, 47 GiB)
| Step | Wall | Peak RSS | Artifact |
|---|---|---|---|
| scip-go 0.2.7 | 0.65 s | 109 MB | index.scip 6.9 MB |
| joern-parse golang (default overlays) | 4.6–4.8 s | 2.2 GB | cpg.bin 4.9 MB |
| joern-export --repr=all --format=neo4jcsv | 2.0 s | 725 MB | 49 MB CSV (10x cpg.bin); CALL 35,207 · CDG 60,847 · REACHING_DEF 238,592 · METHOD 20,262 rows |
Ratio: Joern parse+export is roughly 10x scip-go wall time and 20x its peak RSS (2.2 GB vs 109 MB) on this corpus; the
CSV export alone is 7x the SCIP index. Extrapolating linearly to a 1M-line repo gives minutes and
a ~1.3 GB CSV for Joern versus ~20 s for scip-go, before ReachingDefPass superlinearity.
