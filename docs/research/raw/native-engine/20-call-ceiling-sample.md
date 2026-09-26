# Native-engine research, raw evidence — the honest denominator for `calls`, and the resolution ceiling per repository class

Two evidence stores were read with `sqlite3 -readonly` **only**; the product was never run against
either repository, and both source clones were read without being modified. Every figure is scoped
through the active generation and its unit set.

## 0. Why this file exists

The published call-resolution figures use **every call site** as the denominator, including calls
whose target is a library, the platform or the language runtime — for which "no in-repo definition"
is the correct answer and not a miss. This file measures the denominator nobody had measured: the
share of call sites whose callee **is defined in the repository**. Everything downstream — what the
producers resolve today, what the reachable ceiling is, and what the plan may promise — is restated
against that denominator, **per repository class**, never per repository.

## 1. The two corpora, and the class each one instantiates

| | reference repository | second corpus |
|---|---|---|
| class | **(b)** dynamic/unconfigured: no project configuration for its majority language | **(a)** configured typed: a project configuration present and a precise indexer run over the whole repository |
| files in snapshot | 13,222 (6,663 parsed) | 6,270 (3,812 parsed) |
| majority language by call sites | JavaScript 526,393 (94.8%) | Python 123,029 (90.7%) |
| other languages by call sites | java 26,416 · python 2,291 · typescript 488 | tsx 7,300 · typescript 3,634 · go 1,101 · java 362 · javascript 233 · c 54 · rust 1 |
| tree-sitter `calls` sites / edges | 555,588 / 363,750 | 135,714 / 108,153 |
| syntax tier resolved, **as published** (§8.3 corrects both) | 75,751 (13.6%) | 39,526 (29.1%) |
| engine (`dependence`) `calls` sites | 217,881, of which 40,743 (18.7%) in-repo | 168,376, of which 33,990 (20.2%) in-repo |
| precise units in the active generation | 1 (17 files of one language) | 1, **whole-repository**, 2,990 documents |

The second corpus is what a *configured* repository looks like in the store, and it is the only
place the Section 11.3 call-site join can be measured at scale. The reference repository is what an
*unconfigured* repository looks like. Neither is the definition of a target; each is an instance of
a class — and the second corpus instantiates **both**: it carries a project file for its majority
language and a precise unit that covered 2,990 of its 3,196 files, while its TSX and TypeScript
projects carry a `tsconfig.json` each and still produced no precise unit in that generation and left
no trace of not having done so (§12.5). Its Python leg is therefore read as class (a) and its other
languages as class (b), which is why every figure below is given per leg and never only per corpus.

## 2. The instruments

**Instrument 1 — the compiler index, where one exists (second corpus).** The Section 11.3 join keys
a tree-sitter call site and a precise occurrence on the identical `callsite:<path>:<start>-<end>`
native alias. Both providers publish that alias, so `native_aliases` answers, per call site and
without a human, three questions: is there a compiler-precision occurrence at the callee identifier
(the join), and does the symbol that occurrence names have a **definition** occurrence inside a
repository document (in-repo) or none (external). All 2,990 documents and all 143,369 definition
occurrences of the second corpus resolve to files present in the snapshot, so "has a definition
occurrence" is exactly "defined in this repository".

**Instrument 2 — the hand sample.** Where no compiler index covers the site, the callee is
classified by reading the source at the call site in a read-only clone of the corpus at the exact
commit the snapshot records (`snapshots.head_object_id`, working tree clean in both clones).

**Instrument 3 — the engine verdict (reference repository).** A tree-sitter call site is matched to
an engine `calls` site on exact `(file, start_byte, end_byte)` equality, falling back to an
overlapping range with the same callee name. 129,576 of the engine's 203,896 distinct call-site
ranges match a tree-sitter range exactly (63.6%); a further 26,577 tree-sitter sites overlap an
engine range without equality. **Method limitation:** a sampled site whose engine verdict is
`absent` may be a site the engine lowered to a different range rather than one it did not resolve,
so the engine share measured this way is a lower bound.

## 3. The draw

Deterministic and reproducible: inside each stratum the rows are sorted by `(path, start, end)` and
`random.Random(seed).sample` picks the indices.

| corpus | seed | strata and allocation |
|---|---|---|
| reference repository | **20260916** | javascript 300 of 526,393 · java 90 of 26,416 · python 45 of 2,291 · typescript 25 of 488 = **460 rows** |
| second corpus | **20260917** | unjoined-in-an-indexed-file 90 of 8,091 · unjoined-file-not-indexed 30 of 2,384 · joined-defined 12 of 48,302 · joined-external 12 of 64,252 · TSX/TypeScript 40 of 10,934 · Go/Java/JavaScript/C/Rust 20 of 1,751 = **204 rows** |

Allocation is **disproportionate on purpose**: the overall share is recovered with the stratified
estimator (each stratum weighted by its population share), which is what lets the small strata carry
a per-language statement without distorting the total. All 664 drawn rows passed the offset check —
the callee name recorded in the store appears inside the bytes the range names in the clone — so the
byte positions and the clone agree.

## 4. Classification

| code | meaning |
|---|---|
| `a-repo` | the callee is **defined in this repository** — a function, method or class in a tracked source file |
| `b-lib` | the callee belongs to a **third-party library or framework** the repository depends on |
| `c-platform` | the callee is **platform or runtime**: a built-in, a standard-library function, a DOM or host API |
| `d-unknown` | **undeterminable without executing**: a callee whose identity depends on runtime values, dynamic dispatch on data, or configuration not present in the tree |

`a-repo` is the honest denominator. `b-lib` and `c-platform` are calls for which "no in-repo
definition" is the **correct** answer; counting them in the denominator is what produced the
published 18.7%.

## 5. The sample, aggregated

The 664 classified rows (path, line, call expression, callee, the three instrument verdicts, the
class and a one-line reason each) were read from private corpora, so they are held outside the
repository; every figure below and in §8–§10 is computed from them. Strata are the draw's (§3);
row counts per stratum equal the allocation.

**Class per stratum**

| corpus | stratum | rows | `a-repo` | `b-lib` | `c-platform` | `d-unknown` |
|---|---|---|---|---|---|---|
| reference repository | javascript | 300 | 149 | 73 | 76 | 2 |
| reference repository | java | 90 | 53 | 29 | 8 | 0 |
| reference repository | python | 45 | 6 | 0 | 39 | 0 |
| reference repository | typescript | 25 | 3 | 19 | 3 | 0 |
| second corpus | Python, unjoined-in-an-indexed-file | 90 | 38 | 34 | 17 | 1 |
| second corpus | Python, unjoined-file-not-indexed | 30 | 12 | 0 | 18 | 0 |
| second corpus | Python, joined-defined | 12 | 12 | 0 | 0 | 0 |
| second corpus | Python, joined-external | 12 | 0 | 1 | 11 | 0 |
| second corpus | TSX/TypeScript | 40 | 9 | 26 | 4 | 1 |
| second corpus | Go/Java/JavaScript/C/Rust | 20 | 4 | 3 | 12 | 1 |
| **all** | | **664** | **286** | **185** | **188** | **5** |

**Technique per `a-repo` row** (§9's codes; `T-none` is resolved today)

| corpus | stratum | `T-none` | `T-import` | `T-field` | `T-flow` | `T-hier` | `T-demand` |
|---|---|---|---|---|---|---|---|
| reference repository | javascript | 44 | 6 | 47 | 43 | 7 | 2 |
| reference repository | java | 6 | 5 | 0 | 2 | 40 | 0 |
| reference repository | python | 6 | 0 | 0 | 0 | 0 | 0 |
| reference repository | typescript | 3 | 0 | 0 | 0 | 0 | 0 |
| second corpus | Python, unjoined-in-an-indexed-file | 5 | 2 | 0 | 6 | 25 | 0 |
| second corpus | Python, unjoined-file-not-indexed | 9 | 3 | 0 | 0 | 0 | 0 |
| second corpus | Python, joined-defined | 12 | 0 | 0 | 0 | 0 | 0 |
| second corpus | TSX/TypeScript | 4 | 4 | 0 | 0 | 1 | 0 |
| second corpus | Go/Java/JavaScript/C/Rust | 1 | 2 | 0 | 0 | 1 | 0 |
| **all** | | **90** | **22** | **47** | **51** | **74** | **2** |

## 6. The precise join, measured end to end on the nine-language fixture

The Section 11.3 join is not a projection here: the product was run on the polyglot tools-matrix
fixture — permitted, because a fixture is not a repository — with every pinned analyzer installed,
each indexer invoked through the product's own argv, and the resulting store read with
`sqlite3 -readonly`. Pinned versions at the time of the run: `scip-go 0.2.7`, `scip-typescript 0.4.0`,
`scip-python 0.6.6`, `scip-java 0.13.1`, `scip-clang 0.4.0`, `rust-analyzer 2026-09-14`, with
`clangd 22.1.6`, `gopls 0.23.0`, `jdtls 1.61.0`, `ty 0.0.81`,
`typescript-language-server 6.0.0`, `node 22.23.2` and `jdk 21.0.12.1+1` in the same store —
`tools status` reported **14 entries: 14 installed** and `tools verify` rehashed them against the lock.
The dependence provider was disabled for the run, so nothing below is the engine's.

| fixture project | languages | tree-sitter call sites | joined at compiler precision | joined **and** callee defined in the fixture |
|---|---|---|---|---|
| `c` (a compilation database) | c, cpp | 9 | 8 (88.9%) | 4 |
| `go` (a module) | go | 6 | 5 (83.3%) | 3 |
| `java` (a Maven layout) | java | 6 | **6 (100%)** | 1 |
| `js` (**package.json only, no compiler configuration**) | javascript | 2 | 2 (100%) | 2 |
| `py` (a project file) | python | 4 | 4 (100%) | 3 |
| `rust` (a crate) | rust | 6 | 6 (100%) | 2 |
| `ts` (a tsconfig) | typescript, tsx, javascript | 12 | 10 (83.3%) | 8 |
| **all nine advertised languages** | | **45** | **41 (91.1%)** | **23** |

**The four unjoined sites, named, because four is small enough to name:** `static_cast<int>(…)` in the
C++ file (a keyword operator the grammar records as a call site — it is not a call), `len(…)` in the Go
file (a language built-in), and `require("path")` and `path.join(…)` in a JavaScript file (the host
module loader and a standard-library method). **None of them targets a definition in the fixture.**

**So on this fixture, 23 of 23 call sites whose callee is defined in the repository are resolved at
compiler precision — 100%.** That is the measurement behind the class-(a) target, and it covers every
one of the nine advertised languages rather than one language family.

**Two method notes, recorded rather than smoothed over.**

1. The Java leg needed a `pom.xml` and the C/C++ leg a compilation database, because those are the
   profile triggers; with neither present the two profiles are not planned at all and their languages
   fall back to the syntax tier. This is a **configuration** boundary, not a capability one, and it is
   exactly what separates class (a) from class (b).
2. The C/C++ leg additionally required the compilation database's `directory` fields to be
   **absolute**. The private materialization relocates an absolute `directory` into the copy and
   deliberately leaves a relative one alone, so a hand-written database spelled `"directory": "."`
   reaches the indexer unchanged and it exits 1 in under 20 ms. Both behaviours are as documented;
   the interaction is worth knowing before a corpus is prepared.

## 7. What the producers resolve today, measured on the whole population

The sample bounds the denominator; where a compiler index exists the denominator needs no sample at
all, and the producer shares can be counted over every site rather than estimated. Both tables below
are population counts, not estimates.

**Class (b), the reference repository — every one of its 555,588 tree-sitter call sites.** A site
counts as engine-resolved when an engine `calls` site at the same byte range (or an overlapping range
with the same callee name) names a callee that has a file of its own.

| producer | sites resolved to an in-repo definition | share of all call sites |
|---|---|---|
| syntax tier (in-file or via import) | 75,751 | 13.63% |
| engine, matched onto a tree-sitter site | 38,779 | 6.98% |
| precise tier | 458 | 0.08% |
| **union of all three** | **98,081** | **17.65%** |
| (both syntax and engine) | 16,449 | 2.96% |

Per language: javascript 94,180 of 526,393 (17.89%), java 3,432 of 26,416 (12.99% — the engine's Java
unit failed, so the syntax tier is alone there), python 347 of 2,291 (15.15%), typescript 122 of 488
(25.00%).

**Class (a), the second corpus — its 48,302 compiler-confirmed in-repo-targeted Python call sites.**
This is the honest denominator counted rather than sampled: each of these sites has a compiler-precision
occurrence at the callee identifier whose symbol has a definition occurrence inside a repository file.

| producer | resolves | share of the in-repo-targeted denominator |
|---|---|---|
| syntax tier | 25,226 | **52.23%** |
| engine | 21,179 | **43.85%** |
| syntax ∪ engine | 27,544 | **57.02%** |
| precise join | 48,302 | **100%** |

Read the two together and the shape of the answer is already visible: **without a precise index the
two non-precise producers together reach about three in five of the calls that actually have an
in-repo target, and with one they reach all of them.** The unconfigured corpus is worse than that
because its majority language has no profile at all, not because its calls are harder.

**The name join is not the missing technique.** On the reference repository **70.59%** of all call
sites carry a callee name that some in-repo definition also carries, and among the 82.8% the syntax
tier leaves unresolved the figure is **65.68%**. A resolver keyed on the name alone would therefore
claim two sites in three and be wrong on most of them — which is precisely why the engine's exact
full-name equality join lands at 18.7% of all sites rather than at 70%. Every technique named in §9
is a way of **constraining** that name join with a receiver type, a field table or a hierarchy; none
of them is a way of relaxing it.


## 8. The result: the honest denominator, and what is resolved against it

Stratified estimator throughout; each stratum weighted by its population share, intervals from a
4,000-draw stratified bootstrap. `a-repo` is **the callee's definition is in a file this repository
tracks** — a vendored or bundled dependency's own source counts, because the producers resolve into
those files and a denominator has to admit what the numerator counts.

### 8.1 The honest denominator

| corpus / class | in-repo-targeted (`a-repo`) | 95% interval | in sites |
|---|---|---|---|
| reference repository — class (b), unconfigured | **49.92%** | 44.42 – 55.33 | ≈ 277,300 of 555,588 |
| second corpus, all languages | **40.88%** | 39.62 – 42.22 | ≈ 55,500 of 135,714 |
| …its configured language — class (a) | **42.81%** | 42.04 – 43.57 | ≈ 52,700 of 123,029 |
| …its unconfigured languages — class (b) | **22.15%** | 11.38 – 34.39 | ≈ 2,800 of 12,685 |

The other half of each corpus is `b-lib` (24.65% / 11.63%), `c-platform` (24.79% / 47.16%) and
`d-unknown` (0.63% / 0.33%) — calls for which **"no in-repo definition" is the correct answer**, not a
miss. Per language on the reference repository: javascript 49.67% [44.05, 55.29], java 58.89%
[48.56, 68.49], python 13.33% [6.26, 26.18], typescript 12.00% [4.17, 29.96].

**So the published 18.7% divides by a denominator roughly twice the honest one.** Against call sites
that actually have an in-repo target, the same producers reach the shares below.

### 8.2 The four shares

| resolved by | class (b) — reference repository | class (a) — second corpus's configured language |
|---|---|---|
| syntax tier | **22.85%** [16.70, 29.56] | 25.48% [3.15, 48.83] |
| engine | **10.89%** [6.31, 15.88] | 23.83% [1.21, 46.93] |
| precise join | **0.00%** (one profile ran, over 17 files) | **91.70%** [90.16, 93.40] |
| **union of all three** | **28.60%** [21.98, 35.48] | **94.26%** [92.75, 95.75] |

The class-(b) union is also available as a population count rather than an estimate — §7 gives 98,081
sites, 17.65% of all call sites, which against the 49.92% denominator is 35.4%; the sample's 28.60% is
lower because the sample **withdraws the numerator's own false positives**, which §8.3 measures.

### 8.3 A correction the sample forces on the numerator: `import` is not a resolution

The syntax tier publishes two resolved states. Checked against the hand class:

| tier verdict | reference repository | second corpus |
|---|---|---|
| `in-file` | **36 of 36** are `a-repo` | **16 of 16** are `a-repo` |
| `import` | **9 of 16** | **9 of 38** |

`in-file` is sound in every one of the 52 rows that carry it: the tier found the definition in the same
file. **`import` is not a resolution to a definition at all** — it matches the callee's base name
against an import statement, so `import re`, `import org.testng.Assert`, `from httpx import Response`,
`import typer` and `React.memo` all publish `resolution: import` while **no definition of that name
exists in any tracked file**. Corrected, the syntax tier resolves about **12.4%** of the reference
repository's call sites rather than 13.63%, and about **21.8%** of the second corpus's rather than
29.12%. Every figure of the form "the tree-sitter tier resolves X%" in the program's documents carries
this over-claim and is corrected in doc 14 and doc 20 alongside this file.

### 8.4 Why the reference repository is not a hard case — it is an unconfigured, half-vendored one

Two properties, both measured, and neither a property of its calls:

1. **Unconfigured.** Its majority language, 94.8% of its call sites, has no `tsconfig.json` anywhere in
   the tree, so no precise profile is planned for it and the precise column above is 0.00% by
   configuration rather than by capability.
2. **Half-vendored.** Its dependencies are checked in. 128,205 of its call sites are in one bundled
   front-end template alone, and in the sample **78 of the 83 `a-repo` rows drawn from its second
   JavaScript half, and most of the 66 from the first, are bundle-internal calls**. That is why its honest denominator is as high as 49.92% while only
   about a twentieth of its calls are its own team's code calling its own team's code.

A reader must not carry 49.92% to a repository that installs its dependencies instead of committing
them. The denominator is a property of the repository; the **shares against it** are the transferable
quantity, and those are what the targets are set on.

## 9. What would resolve the rest, technique by technique, language family by language family

Every `a-repo` row carries the technique that would resolve it. The shares are of the in-repo-targeted
population, so they sum to 100% and the cumulative column is the reachable ceiling after that
technique is added.

| technique | class (b) — reference repository | cumulative | class (a) — configured language | cumulative |
|---|---|---|---|---|
| `T-none` — resolved today | 28.60% [21.55, 35.71] | 28.60% | 93.91% [92.37, 95.42] | 93.91% |
| `T-import` — follow the module edge to a definition (not the name match of §8.3) | 4.32% [1.62, 7.71] | 32.93% | 0.79% | 94.71% |
| `T-field` — field-based resolution: property name → the repository definitions bound to it | **29.73%** [23.05, 37.15] | 62.66% | 0.00% | 94.71% |
| `T-flow` — flow-based type inference through assignments, parameters, returns | **27.41%** [20.56, 34.13] | **90.07%** | 1.02% | 95.73% |
| `T-hier` — class-hierarchy analysis through a declared or inferred repository type | 8.66% [5.54, 12.43] | 98.73% | **4.27%** [2.92, 5.59] | **100%** |
| `T-demand` — demand-driven resolution at read time | 1.27% [0.00, 3.22] | 100% | 0.00% | 100% |

**Each technique is a rule per language family, not per repository.**

| technique | language families it is stated for | the nine advertised languages it applies to |
|---|---|---|
| `T-import` | every family with a module or package system | all nine — ES modules and CommonJS (javascript, typescript, tsx), `import`/`from … import` (python), package imports (go, java), `use` (rust), `#include` (c, cpp) |
| `T-field` | families where a callable is stored in a named property of an object, prototype or class body | javascript, typescript, tsx, python primarily; degenerate but sound for java, go, rust, c, cpp, where a member name is already a declared member |
| `T-flow` | every family — it is the intraprocedural forward propagation over the SSA def-use chains decision 2 already builds, assigning each value a candidate set | all nine. It is the **only** technique that supplies a receiver type in the dynamic families, and it is what makes `T-field` and `T-hier` applicable at all |
| `T-hier` | families with a subtype relation | java, typescript, tsx (classes and interfaces), python (classes and `Protocol`), c++ (virtual dispatch), rust (traits), go (interfaces); inapplicable to c |
| `T-demand` | every family — it refines only the site a query names, so it carries no index-time budget | all nine |

**The name join is not among them, and §7 says why:** 70.59% of the reference repository's call sites
carry a callee name that some tracked definition also carries, so a resolver keyed on the name alone
claims two sites in three and is wrong on most. Every technique above **constrains** that join with a
receiver type, a field table or a hierarchy.

## 10. The targets these measurements support

**Class (a) — a configured typed repository** (a project configuration present and the precise
indexer run): **≥ 95% of in-repo-targeted call sites resolved at compiler precision through the
existing call-site join.** Supported, and the qualification is named rather than buried:

- On the nine-language fixture, where every project is configured and every dependency is resolvable,
  the join resolves **23 of 23** in-repo-targeted call sites — **100%**, across all nine languages.
- On the second corpus's configured language the join alone reaches **91.70%** and the union of the
  three producers **94.26% [92.75, 95.75]** — at the edge of the target, not past it. The gap is
  **10,475 sites the precise unit did not answer for**, and the sample says what they are: 2,384 in
  files the indexer never indexed, and 8,091 in files it did index where **all 90 sampled rows are
  attribute calls whose receiver the indexer could not type** — a third of them members of packages
  absent from the index environment, a third `Any`-typed receivers, a third repository objects reached
  through an un-inferred binding.
- The residue's own composition closes it: **4.27% of in-repo-targeted sites need class-hierarchy
  analysis through a declared repository type** and 1.02% flow inference, which takes the cumulative to
  **100%**. And the 94.26% union decomposes as **91.70 pp from the join**, 0.91 pp from the sound
  `in-file` state and **1.31 pp from the `import` state** that §8.3 measures at 9 of 38 — so the
  defensible floor for what is resolved today on this class is **92.61%**, which is below the target
  by more than the inference is worth guessing about. So ≥ 95% is honest **for the join plus the index-time inference of this plan**, and is
  **not** honest for the join alone on a repository whose precise unit skips files or runs without the
  dependency environment. The target is stated on the pair.

**Class (b) — a typed repository without configuration, or a dynamic-language repository:** **≥ 85% of
in-repo-targeted call sites resolved by language-general inference.** Supported, and the arithmetic is
the table in §9: 28.60% is resolved today, field-based resolution adds **29.73 pp** and flow-based
inference **27.41 pp**, which reaches **90.07%** — above the target with the interval's lower end at
roughly 80% and class-hierarchy analysis a further 8.66 pp in reserve. **The two techniques that carry
the target are named and neither is optional.** The last **1.27%** is `d-unknown` plus the sites whose
callee identity depends on a call-site-specific value; those are the demand-driven residue and are the
reason the target is 85% and not 100%.

## 11. Method limitations, stated rather than smoothed over

1. **The engine verdict is a lower bound.** 129,576 of the engine's 203,896 distinct call-site ranges
   match a tree-sitter range exactly (63.6%); a sampled site whose engine verdict is `absent` may be a
   site the engine lowered to a different range.
2. **The `T-hier`/`T-flow` boundary is a convention, and it is the largest single lever on §9's
   histogram.** Fixed across all four blocks as: a receiver with a **declared** repository type on a
   field, local or parameter is `T-hier`; a receiver that is a **chained return value** is `T-flow`.
   A reviewer using "any receiver type that must be inferred is `T-flow`" would move on the order of
   40 rows between the two. It does not move the cumulative ceiling, only the split.
3. **`T-none` is mechanical**, read off the instruments rather than re-judged, so a row the engine
   resolved by a name join that happens to be right still counts as resolved today. Two rows in the
   reference sample carry that caveat in their own cell.
4. **The small strata are indicative.** typescript on the reference repository is 25 rows of a
   488-site stratum weighing 0.09%; its per-language interval is wide and it moves the overall
   estimate by hundredths of a point.
5. **The rows themselves are not in the repository.** They quote paths and source from private
   corpora, so §5 carries their aggregates; every row of the seeded draw was classified and none was
   dropped, because dropping one would break the draw.
6. **Two corpora, one host, one engine version, one set of pinned indexer versions.** Re-measure when
   any of them changes.

## 12. Corrections this file makes to the program's earlier records

1. **`14-store-counts-reference.md` Q5.2 — "scip-java produced no occurrence at all at the call site" is
   wrong.** Run through the product's own argv on a Maven-layout fixture, scip-java 0.13.1 joins
   **6 of 6** Java call sites at compiler precision. The earlier finding came from a different fixture.
2. **`14-store-counts-reference.md` Q5.4 — "SCIP cannot help" with call-through-a-value is wrong at scale.**
   What remains true is that a call *edge* is not derivable from occurrence roles; the call **site**
   must come from the grammar. But the *join* works precisely on those sites: on the second corpus
   **79,154 of 95,780 syntax-unresolved call sites (82.6%) carry a compiler-precision occurrence at
   the callee identifier**.
3. **"A perfect precise run over this repository reaches roughly one call site in twenty" is a
   property of that repository's configuration, not of the precise tier.** The same tier covers
   **112,554 of 135,714 call sites (82.9%)** on a corpus whose majority language carries a project
   file, **91.5%** of that language's own sites, and **41 of 45** on the nine-language fixture.
4. **The syntax tier's `import` state is not a resolution** — §8.3.
5. **The per-scope observability gap recorded as Correction 3 in doc 14 reproduces on the second
   store.** Its `scip` capabilities are `fresh` with empty details while only one of its several
   eligible profiles produced a unit; the profiles that did not run leave no `units` row, no
   `provider_runs` row and no per-scope capability row. The gap is a property of the product, not of
   either corpus.
