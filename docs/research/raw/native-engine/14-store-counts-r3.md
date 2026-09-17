# Native-engine research, raw evidence — call resolution measured on the r3-1 and r3-3 stores

Both stores read with `sqlite3 -readonly` only; the `codectx` binary was never invoked. Every figure
is scoped through the **active generation** and its unit set.

## Schema columns relied on

`active_generations(repository_id, generation_id)`; `generations(id, snapshot_id, status, health)`;
`generation_units(generation_id, provider_id, scope_key, unit_id, carried)`;
`generation_capabilities(generation_id, provider_id, capability, scope_key, state, diagnostic_code, details_json)`;
`units(id, provider_id, provider_version, scope_key, state, origin_run_id)`;
`provider_runs(id, generation_id, provider_id, status, counters_json, diagnostic_code, started_at, completed_at)`;
`relation_facts(unit_id, relation_id)`; `relation_ids(id, from_node_id, kind, to_node_id)`;
`node_facts(unit_id, node_id, language, name, qualified_name, file_id, metadata_json)`;
`node_ids(id, kind)`; `evidence(unit_id, relation_id, node_id, precision, file_id, start_byte, end_byte, detail)`;
`unit_inputs(unit_id, file_id)`; `snapshot_files(snapshot_id, file_id, language, size_bytes)`; `files(id, path)`.

Load-bearing discoveries: **relation kind lives in `relation_ids.kind`, not in `relation_facts`**.
**Provider identity comes only from `generation_units.provider_id`** (`relation_facts` has no provider
column). **Precision is `evidence.precision`; resolution state is `evidence.detail`**, mirrored as
`node_facts.metadata_json` on the *callee* node (`{"callee":…,"candidates":N,"resolution":…}`).
`fact_keys` is populated **only** by the dependence provider (3,518,398 rows, r3-3 gen 3) and is empty
for treesitter and scip. `evidence` rows are call **sites** (one per byte range); `relation_facts` rows
are de-duplicated caller→callee **edges**. Both are reported.

## Active generations

| store | repo | active gen | status/health | snapshot | units in gen |
|---|---|---|---|---|---|
| r3-3 | the r3 corpus | **3** | active / degraded | `5ee76b50…` | filesystem 13222, treesitter 6663, manifest 153, dependence 10, **scip 1** = 20,039 |
| r3-1 | same repo, same snapshot | **3** | active / **fresh** | `5ee76b50…` | filesystem 13222, treesitter 6663, manifest 153, dependence 10, **scip 0** = 20,038 |

`carried=0` on every unit in both stores.

## Main aggregate query (run once per store)

```sql
WITH gu AS (SELECT unit_id, provider_id FROM generation_units WHERE generation_id = 3)
SELECT gu.provider_id, ri.kind, e.precision,
       CASE WHEN ri.kind IN ('calls','references','may_refer_to') THEN e.detail ELSE '' END AS detail,
       count(*) AS sites, count(DISTINCT e.relation_id) AS edges
FROM evidence e
JOIN gu ON gu.unit_id = e.unit_id
JOIN relation_ids ri ON ri.id = e.relation_id
GROUP BY 1,2,3,4 ORDER BY 1,2,5 DESC;
```
(12.1 s on r3-3, 43.6 s on r3-1, no index built.)

## Q1 — tree-sitter (syntax) call sites, gen 3, identical in both stores

| callee state (`evidence.detail` = `metadata_json.resolution`) | sites | edges |
|---|---|---|
| unresolved (`candidates`:0) | 459,791 | 302,319 |
| resolved in-file (metadata `{}`) | 60,734 | 40,773 |
| resolved via import (`candidates`:0) | 15,017 | 10,239 |
| ambiguous (`candidates` 2…67+) | 20,046 | 10,419 |
| **total `calls`, precision=`syntax`** | **555,588** | **363,750** |

Resolved = 75,751 sites (**13.6%**) *as the store labels them* — but that is **two states, not one**:
60,734 `in-file` (**10.9%**), which the sample finds sound, plus 15,017 `import` (2.7%), which matches
the callee's base name against an import statement and mostly does **not** reach a definition.
Corrected, the tier resolves about **12.4%** — C4 and C6 below. Unresolved + ambiguous = 479,837
(**86.4%**). Ambiguous candidate sets are a separate kind: `may_refer_to` / detail
`ambiguous call target` = **179,626 sites / 92,598 edges** (≈8.96 candidates per ambiguous site).

By language (`snapshot_files.language`): javascript 3,748 files / **526,393 sites** (434,432 unres /
60,346 local / 11,569 import / 20,046 ambig); java 210 / 26,416 (22,984 / 129 / 3,303 / 0); python
35 / 2,291 (1,954 / 208 / 129 / 0); typescript 13 / 488 (421 / 51 / 16 / 0). r3-1 returns the identical
table — same snapshot, same treesitter units.

## Q2 — engine (`dependence`, 4.0.627) `calls`, precision `static_analysis`

Resolution state is on the callee `node_facts`: `metadata_json.resolution='import'` **and
`file_id IS NULL`** = external/library stub (`__ecma.Array.factory`, DOM/Node type stubs); metadata
`{}` **and `file_id NOT NULL`** = a real in-repo definition. `candidates` is always 1 on dependence
callees.

| store | total sites | total edges | → in-repo definition | → import stub |
|---|---|---|---|---|
| r3-3 | 217,881 | 115,940 | **40,743 sites / 27,897 edges (18.7% / 24.1%)** | 177,138 / 88,043 |
| r3-1 | 212,364 | 112,858 | **40,326 sites / 27,563 edges (19.0% / 24.4%)** | 172,038 / 85,295 |

By scope-key family (all 10 units are `workspace`-capability units):

| family | r3-3 sites (in-repo) | r3-1 sites (in-repo) |
|---|---|---|
| `pkg:javascript:` × 9 units | 214,820 (40,470) | 209,303 (40,053) |
| `pkg:python:` × 1 unit | 3,061 (273) | 3,061 (273) |
| `pkg:java:…` | **absent — unit failed, 0 records** | **absent — unit failed, 0 records** |

Largest single unit both times: `pkg:javascript:app` — r3-3 136,447 sites / 68,713 edges (18m10s,
1,530,633 records); r3-1 130,930 / 65,631 (13m55s, 1,474,886 records).

Other engine kinds, r3-3 → r3-1: `data_flows_to` 1,983,374/1,060,579 → 1,945,863/1,038,455;
`reads` 218,827/108,540 → 214,991/106,199; `writes` 153,194/129,751 → 150,617/127,407;
`control_depends_on` 101,814/59,850 → 99,401/58,284.

## Q3 — the precise (SCIP) provider — three corrections to the program ledger

**r3-3, read out of the store, not inferred.** Exactly one `scip` unit exists: `units.id=20046`,
`provider_id='scip'`, **`scope_key='profile:scip-python:QA/RobotTests'`**,
`provider_version='4/7245a5ed…'`, `state=sealed`, `source_binding=verified`. Its `provider_runs` row:
`status=succeeded`, `counters_json={"records_emitted":4644,"bytes_processed":528196}`,
19:39:16.86→19:39:41.13 = **24.3 s**.

- Language: **Python** (scip-python). **Not TypeScript.**
- Facts: **1,099 `references` edges — and zero `calls` of any kind.** 929 node facts (703 variable,
  85 method, 53 function, 38 field, 29 class, 17 file, 4 namespace). 2,833 `evidence` rows at
  `precision='compiler'`: 2,144 `detail='reference'`, 692 `'definition'`, 17 `'document'`.
- Coverage: **17 files, all 17 Python files under `QA/RobotTests/`** (119,214 bytes). Tree-sitter
  recorded **645 call sites / 474 call edges in 16 of those 17 files** (485 unresolved, 89 local,
  71 import) = **0.116% of the repo's 555,588 call sites**, 28.2% of its 2,291 Python call sites.

**Correction 1:** the one unit that ran was **scip-python**, not one of ten TypeScript units.
**Correction 2:** `CTX_PROVIDER_OUTPUT_INVALID` in r3-3 belongs to the **dependence** provider, not a
precise unit. `provider_runs` has one `provider_id='dependence'` row with `status='failed'`,
`records_emitted:0`, 8.7 s; `generation_capabilities` (gen 3, all five dependence capabilities
`partial`) carries
`details_json={"scope_key":"pkg:java:QA/SeleniumWebdriver/TestngExtentFramework","units_failed":"1"}`.
**There is no Java precise unit anywhere in either store.**
**Correction 3 (a method gap AND a product observability defect; it reproduces on a second store —
C5):** the nine unavailable precise units
leave **no trace at all** — no `units` row, no `provider_runs` row, and `generation_capabilities` is
keyed at `scope_key='workspace'` only, with `details_json={}` for scip. The "9 of 10 failed
`CTX_PROVIDER_UNAVAILABLE` with 0 records" claim is **not checkable against the store**; it can only
come from the run log. Making it checkable needs a per-scope `generation_capabilities` row, or a
`provider_runs` row emitted for unavailable providers.

**r3-1 — the program ledger verified, no correction needed.** Zero scip units, zero scip runs. Gen-3
capabilities are exactly **5 fresh / 8 unavailable**: fresh = filesystem `search`+`structure`,
manifest `manifests`+`documentation`, treesitter `structure`; unavailable = dependence ×5
(`CTX_PROVIDER_UNAVAILABLE`, `{"reason":"units_deferred"}`) and scip
`precise_definitions`/`precise_references`/`precise_implementations` ×3. One caveat:
The r3-1 run log prints "3 fresh, 1 partial, 9 unavailable" — that is an *earlier*
generation's status line, not active gen 3. Cite the store's 5/8. Note also that **r3-1's 10
dependence units are sealed and carry all their facts, yet the capability is
`unavailable`/`units_deferred`** — the facts are in the store but not published.

## Q4 — coverage projection from file counts per project

Candidate profiles determined from the build files the six indexers require, located via
`snapshot_files`+`files` on snapshot `5ee76b50…`. The repo has `QA/RobotTests/requirements.txt` and
`QA/SeleniumWebdriver/TestngExtentFramework/pom.xml`; it has **no `tsconfig.json` anywhere**, no
`pyproject.toml`, no `go.mod`, no `Cargo.toml`, no `compile_commands.json`.

| candidate precise unit | build file | project files (its lang) | % of 6,663 parsed | measured call sites | % of 555,588 |
|---|---|---|---|---|---|
| scip-python @ `QA/RobotTests` (**ran**) | requirements.txt | 17 py | 0.26% | 645 | 0.116% |
| scip-java @ `QA/SeleniumWebdriver/TestngExtentFramework` | pom.xml | 223 java | 3.35% | 26,416 | 4.754% |
| scip-typescript @ `Meteor3preUpgradeScripts/meteor-async-migration` | **none** | 17 ts | 0.26% | 488 | 0.088% |
| scip-python @ 4 other py projects (QA/SeleniumWebdriver 48, Meteor3pre 8, playwright-validation 5, cijobs 5) | **none** | 66 py | 0.99% | 1,646 | 0.296% |
| scip-go / rust-analyzer / scip-clang | **none** | 0 | 0 | 0 | 0 |
| **all planned precise units, had they run** | | **323 / 6,663** | **4.85%** | **29,195** | **5.25%** |

**Assumption stated plainly:** the file-count projection assumes call sites are uniformly distributed
across the files of a language. It **over-states** coverage where a failed project is test-heavy
(`QA/RobotTests` averages 38 call sites/file, three times sparser than the repo mean of 139) and
**under-states** it where the project is dense application code (`app/client` averages 136/file). Here
the store lets me replace the assumption with a measurement: uniform-by-files gives 4.85%, actual
per-project call-site counts give 5.25% — they agree within 0.4 pp only because Java (126 sites/file)
happens to be almost as dense as JavaScript (140 sites/file).

**Range:** restricting to profiles whose build file actually exists → (645 + 26,416)/555,588 =
**4.87%**; admitting every language-plausible profile → **5.25%**. **A perfect SCIP run over this
repository reaches roughly one call site in twenty** — which is a property of *this repository's*
configuration, not of the precise tier (C3).

## Q5 — where coverage is lost under SCIP + LSP

`docs/research/08-scip-empirical-six-indexers.md` (2026-09-13, linux/amd64, decoded with
`scip print --json`, scip CLI 0.10.0) measures six indexers on one fixture
(`counter = helper(counter)`, then `f = helper`, then `f(2)`):

| indexer | version | call-site `helper` roles | function-value `helper` roles | assignment target `counter` | typed_range |
|---|---|---|---|---|---|
| scip-go | 0.2.7 | 8 (ReadAccess) | 8 | 8 (ReadAccess, **no WriteAccess**) | no |
| scip-typescript | 0.4.0 | **0** | 0 | 0 | no |
| scip-python | 0.6.6 | 8 | 8 | 8 | no |
| rust-analyzer scip | 1.98.0 | **0** | 0 | 0 | yes |
| scip-java | 0.13.1 | **absent (0)** | absent | absent | yes |
| scip-clang | 0.4.0 | **0** | 0 | 0 | no |

**The `scip-java` row is a property of that fixture, not of the indexer (C1).** Re-run on a
Maven-layout fixture through the product's own argv, scip-java 0.13.1 joins **6 of 6** Java call sites
at compiler precision.

**No indexer emits WriteAccess (0x4)** — `counter = …` is ReadAccess or 0 in all six, so SCIP role bits
cannot back `reads`/`writes`. **Call edges are not derivable from occurrences**: roles are identical
for `helper(counter)` and `f = helper`, so no indexer distinguishes a call site from a non-call
reference; the call *site* must come from tree-sitter's `@call.name` range and SCIP only resolves the
callee occurrence at that range.

Structural gaps, with store support:

1. **JavaScript — the whole loss.** 6,340 files, 3,748 with calls, **526,393 call sites = 94.75% of
   all**. The only candidate indexer is scip-typescript, which measured **0 roles on the call site**,
   and the repo has **no `tsconfig.json` at all**. Meteor global-namespace and template-helper dispatch
   is not type-checkable without tsconfig + node_modules.
2. **Java.** 223 files / **26,416 call sites**. `pom.xml` exists and scip-java would run; **no Java
   precise unit ran in either store** (Q3). The earlier reading of this row — that scip-java produces
   no occurrence at a call site and a clean run would yield nothing to join to — is **wrong** (C1): on
   a Maven-layout fixture, through the product's own argv, scip-java 0.13.1 joins **6 of 6** Java call
   sites at compiler precision. In both stores Java also has zero engine edges (the dependence java
   unit failed), so all 26,416 sites are syntax-only today — for want of a unit, not of an indexer.
3. **Dynamic/templating languages with no indexer and no grammar.** From the extension histogram over
   the 5,066 files with `snapshot_files.language=''`: **247 `.robot` + 156 `.resource`** (Robot
   Framework test logic) and **546 `.jade`** Blaze templates that call helpers. 0 call sites recorded
   today; SCIP adds none.
4. **Call-through-a-value — the largest gap, and the store sizes it.** **459,791 of 555,588 call sites
   (82.8%)** carry `{"candidates":0,"resolution":"unresolved"}` — the callee is a member access or a
   value (`.map`, `.then`, `.filter`, `.click`). Doc 08's conclusion 1 is about the call *edge*, which
   occurrence roles cannot carry; read as "SCIP cannot help" it is **wrong at scale** (C2). On a second
   corpus **79,154 of 95,780 syntax-unresolved call sites (82.6%)** do carry a compiler-precision
   occurrence at the callee identifier. A further 20,046 sites (3.6%) are `ambiguous` with 179,626
   candidate edges.
5. **C/C++ macro call sites and Go build-tag-excluded files** are real structural gaps but carry **zero
   numeric weight in this repo** — `snapshot_files` has no c/cpp/go/rust rows. Doc 08 supports them
   indirectly: scip-clang needs `compile_commands.json` (macro-expanded sites vanish from the expanded
   TU) and scip-go indexes one build configuration.
6. **Kinds SCIP has no representation for at all**, lost outright: `control_depends_on` 101,814 sites /
   59,850 edges and `data_flows_to` 1,983,374 / 1,060,579 (r3-3 gen 3), plus `reads` 218,827/108,540
   and `writes` 153,194/129,751 that the WriteAccess finding rules out.

## Corrections this record takes from `20-call-ceiling-sample.md`

That file measures the denominator this one never had — the share of call sites whose callee is
**defined in a file the repository tracks** — over a seeded, language-stratified hand sample of this
store and a second one (460 rows and 204 rows, classified from the source at each byte range in a
read-only clone at the snapshot's own commit), plus one product run on a nine-language fixture with
every pinned indexer installed and invoked through the product's own argv. Nothing below was measured
by running anything against *this* repository; both stores here are still read-only. Numbered C1–C6 so
as not to collide with Q3's Corrections 1–3.

**C1 — Q5.2's "scip-java produced no occurrence at all at the call site" is wrong.** That was doc 08's
one-file fixture. Run through the product's own argv on a Maven-layout fixture, **scip-java 0.13.1
joins 6 of 6 Java call sites at compiler precision**, 1 of them to a definition the fixture holds. The
Q5 table records what doc 08's fixture produced and is not corrected as a measurement; what is wrong is
the inference drawn from it. Java's 26,416 sites here are syntax-only because **no Java precise unit
ran** (Q3), not because a run would have yielded nothing to join to.

**C2 — Q5.4's "SCIP cannot help" with call-through-a-value is wrong at scale.** What survives is the
narrow claim, and it survives intact: a call *edge* is not derivable from occurrence roles, so the call
**site** must come from tree-sitter's `@call.name` range. The *join* onto that range works precisely on
these sites — on a second corpus, **79,154 of 95,780 syntax-unresolved call sites (82.6%) carry a
compiler-precision occurrence at the callee identifier**. The 459,791 unresolved sites counted in Q1
are unresolved because no precise unit covers their files, not because there is nothing there to
resolve them with.

**C3 — Q4's "a perfect SCIP run over this repository reaches roughly one call site in twenty" is a
property of this repository's configuration, not of the precise tier.** The same call-site join covers
**112,554 of 135,714 call sites (82.9%)** on a corpus whose majority language carries a project file —
**91.5%** of that language's own sites — and **41 of 45** on the nine-language fixture, including **23
of the 23** whose callee the fixture defines. 4.87–5.25% is what a missing `tsconfig.json` costs across
94.75% of *this* repository's call sites (Q5.1). It is not a ceiling any other repository inherits, and
it must not be quoted as one.

**C4 — the syntax tier publishes two resolved states and only one of them is a resolution.** Checked by
hand against the source at the byte range: `in-file` is sound in **52 of 52** sampled rows across both
corpora — the tier really did find the definition in the same file. `import` reaches no definition at
all. It matches the callee's **base name** against an import statement, so `import re`, `import
org.testng.Assert`, `from httpx import Response`, `import typer` and `React.memo` all publish
`resolution: import` while **no definition of that name exists in any tracked file**. Sampled, it is
right **9 times in 16** on this repository and **9 times in 38** on the second corpus. Corrected for
that soundness rate the tier resolves about **12.4%** of this repository's call sites and about
**21.8%** of the second corpus's.

**C5 — Q3's Correction 3 is not a property of this store.** The per-scope observability gap reproduces
on the second store: its `scip` capabilities are `fresh` with empty `details_json` while only one of
several eligible profiles produced a unit, and every profile that did not run leaves **no `units` row,
no `provider_runs` row and no per-scope `generation_capabilities` row**. Two stores, two corpora, the
same hole — it is a product defect rather than a corpus accident, and the remedy Correction 3 names (a
per-scope capability row, or a `provider_runs` row for unavailable providers) stands unchanged.

**C6 — Q1's "Resolved = 75,751 sites (13.6%)" is therefore two figures reported as one.** 60,734 of
them (**10.9%**) are `in-file` and sound; 15,017 (2.7%) are `import` and mostly not resolutions at all.
The edge column splits the same way: 40,773 `in-file` to 10,239 `import`. Every document that cites
this record took 13.6% as a single resolved bucket, and that is the figure that propagated. Read the
row as **10.9% sound, about 12.4% corrected, 13.6% as the store labels it**; Q1 above is annotated
accordingly. The per-language breakdown in Q1 carries the same split and the same caveat — javascript
60,346 local against 11,569 import, java 129 against 3,303, python 208 against 129, typescript 51
against 16 — and Java's row is where it bites hardest, since 3,303 of its 3,432 syntax-resolved sites
are `import`.

## Figures that could not be obtained

- Per-profile precise-unit failure records — **no schema column exists**
  (`generation_capabilities.scope_key` is `'workspace'` for scip; unavailable providers write no
  `units`/`provider_runs` rows).
- Any LSP-derived measurement — no LSP provider appears in either store (it is an ephemeral overlay,
  by design).
