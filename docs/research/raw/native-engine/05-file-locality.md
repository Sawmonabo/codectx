# Native-engine research, raw evidence — the file-locality measurement (the strongest evidence for a per-file native engine)

Source: `docs/research/11-incremental-joern.md` §4.3 **[MEASURED]**, and `docs/research/14-incremental-synthesis.md` §2.
Method (quoted): each unit was parsed with all but one file excluded, keeping the real unit root,
then facts were restricted to the methods declared in that file and compared with the whole-unit run.

```
### 4.3 File-only and package-only parses: what survives **[MEASURED]**

For each language the unit was parsed with all but one file excluded (and, for Python/TypeScript, with
all but one package/directory excluded), keeping the real unit root so module paths and relative file
paths are unchanged. Facts were then restricted to the methods *declared in that file* and compared
with the same methods from the whole-unit run.

Cost:

| Run | files parsed | parse wall | parse RSS | export wall | export RSS |
|---|---|---|---|---|---|
| Go whole | 173 | 4.77 s | 1.35 GB | 1.98 s | 0.71 GB |
| Go one file | 4 | **2.53 s** | 0.34 GB | 0.60 s | 0.16 GB |
| Python whole | 475 | 8.23 s | 3.30 GB | 6.19 s | 0.87 GB |
| Python one file | 2 | **2.46 s** | 0.35 GB | 0.75 s | 0.23 GB |
| Python one package (27 files) | 28 | 2.94 s | 0.62 GB | 1.07 s | 0.48 GB |
| TypeScript whole | 66 | 6.72 s | 2.29 GB | 2.64 s | 0.75 GB |
| TypeScript one file | 2 | **4.44 s** | 0.78 GB | 1.00 s | 0.44 GB |
| TypeScript one directory (14 files) | 14 | 5.13 s | 1.29 GB | 1.40 s | 0.51 GB |
| C whole (3 files) | — | 1.72 s | 0.24 GB | — | — |
| C one file | — | 1.72 s | 0.24 GB | — | — |
| Java whole (2 files) | — | 4.98 s | 0.51 GB | — | — |
| Java one file | — | 4.66 s | 0.51 GB | — | — |

Fidelity, for the methods declared in the edited file:

| Language / scope | METHOD rows | CDG | REACHING_DEF | REF | non-operator CALL edges, identical key | node types wrong |
|---|---|---|---|---|---|---|
| Go, file only | 43/43 identical | 2,584/2,629 (98.3%), 15 spurious | 6,754/6,857 (98.5%), **960 spurious (+14%)** | 932/932 (100%) | 174/240 (**72%**) | 380 of 2,554 (**14.9%**) |
| Python, file only | 39/39 identical | 935/935 (**100%**) | 5,112/5,112 (**100%**) | 914/955 (96%) | 236/377 (63%) | 54 of 1,478 (3.7%) |
| Python, package only | 39/39 identical | **100%** | **100%** | 953/955 (99.8%) | 282/377 (75%) | 1 of 1,478 (0.1%) |
| TypeScript, file only | 205/205 identical | 8,750/8,750 (**100%**) | 31,571/31,571 (**100%**) | 3,972/3,972 (100%) | 1,170/1,556 (75%) | **0** |
| TypeScript, directory only | 205/205 identical | **100%** | **100%** | 100% | 1,272/1,556 (82%) | 0 |
| C, file only | identical | 22/22 (100%) | 95/95 (100%), 1 spurious | 17/17 (100%) | 2/4 (50%) | 0 |
| Java, file only — **exclusion had no effect, see below** | identical | 24/24 (100%) | 101/101 (100%) | 13/13 (100%) | 4/4 (100%) | 0 |

The call-edge column above counts only edges whose key is byte-identical. That is a lower bound on
what is usable, because it scores an edge as lost when the callee merely became an external stub
carrying the *same* `METHOD_FULL_NAME` — which the plan's reconciler aliases back to the real
declaration (report 07 §"Sharding experiment"). Splitting the same data by what actually happened,
and separating calls *out of* the re-parsed file from calls *into* it from files that were not
```

## Why this matters for a native engine

The engine itself, asked to analyse one file instead of a whole project, still produces:

| language | CDG kept | REACHING_DEF kept |
|---|---|---|
| Python | **100%** | **100%** |
| TypeScript | **100%** | **100%** |
| C | 100% | 100% (1 spurious) |
| Java | 100% | 100% |
| Go | 98.3% (15 spurious) | 98.5% (960 spurious, +14%) — the per-package `<clinit>` artifact |

So the two fact families a native engine would compute are **already demonstrated to be file-local**,
by the reference implementation, on real code. The project-wide JVM run is not buying dependence
precision; it is buying *call* resolution, which the same table shows collapsing to 75% (TypeScript,
and 19% of those are **wrong**, not merely unresolved) — the fact family a precise index owns.

Go is the exception and the reason is named: package-level declarations are merged into a synthetic
per-package `<clinit>`, so a file-only Go parse invents 960 spurious REACHING_DEF edges (11 §4.3,
and the per-frontend table at 11:474). A native Go implementation with a real package scope does not
inherit that artifact — it is a property of how gosrc2cpg fakes package initialisation, not of Go.
