# Native-engine research, raw evidence — the parity asymmetry and the oracle's error band

All figures already measured by the product's own research; reproduced here verbatim so the
post-MVP report can cite them without re-running anything. Source: `docs/research/10-round3-empirical.md`.

## §8 — what subdividing a unit costs (the asymmetry that decides the design)
```
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

```

**Reading.** The facts that survive subdivision (CDG 99.7%, REACHING_DEF 99.9%, methods 100%) are
exactly the facts an intraprocedural native engine would produce; the fact that collapses (calls
resolved to internal methods: 46% on TypeScript, 79% on Python) is exactly the one a precise index
replaces. The split in the data is per fact family, not per language.

## §9b — the run-to-run variance band every parity claim is bounded by
```
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

```

**Consequence for a differential oracle.** `docs/providers-dependence.md` states that a claim of
*equality* between two engine runs anywhere in the repository is a defect. So a native-vs-engine
oracle can only ever assert a band, and the band must be measured from two engine runs on the same
corpus before it can judge a third, native run.

## §9a — the definition cap, which a native engine inherits as a design question
```
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

```

## The differential oracle already exists in the product (this is the finding, not a proposal)

`docs/providers-dependence.md` §Refresh and delta, quoted:
```

What is wired today, exactly. Every import derives an engine-id-independent
semantic key per fact (the fact label, its owning method's full name, its
file, the operator it was lowered from, its target name, its ordered byte
ranges and — for a relation — its two endpoints' published identities;
`<clinit>`-owned facts use a digest of their endpoints' source text instead of
their coordinates, because the Go frontend shuffles those between two parses
of identical source). The endpoints are part of the key because a relation's
published identity is derived from them and a located declaration's identity
is derived from its declaration range: edit a callee and every call edge into
it moves to a new identity while the site's own file, owner, target name and
byte range do not move at all. Without them one key would name two different
facts across a refresh, the previous row would be carried under a key the
fresh run still publishes, and the unit would hold an edge whose endpoint no
longer exists. A node fact takes no endpoints component — its identity
already tracks through its file and its coordinates, and an identity minted
from a declaration range would reintroduce the initializer nondeterminism the
coordinate rule exists to defeat. One measured pair of independent engine runs
over one unchanged package of this repository published the same 6842 keys,
none changed and none removed — one sample, not a determinism guarantee: the
engine is not run-to-run deterministic (see §The engine for its variance
band), and what this measures is that the variance did not reach the key
algebra on that pair. The importer streams that key set to a sorted file,
diffs a supplied previous set against it in one merge pass, and can publish
only the relations whose key changed. Deriving the keys measured 1–5 s per
unit against 17–65 s engine runs, and a one-line edit changes about one fact
row in ten thousand.

Every fact reaches storage with its keys. A published fact carries *every*
```

**Reading.** The product already computes, per fact, an **engine-id-independent semantic key**
(label, owning method full name, file, source operator, target name, ordered byte ranges, and for a
relation both endpoints' published identities), streams the key set to a sorted file and diffs two
sets in one merge pass. That is precisely the comparator a native-vs-engine differential oracle
needs, and it is already built, already proven on a pair of independent engine runs (6,842 keys,
none changed, none removed on one unchanged package), and already deliberately independent of the
engine's node ids and of the Go frontend's `<clinit>` coordinate shuffling.

A native engine is therefore a **second producer feeding an existing key algebra**, not a new
comparison framework. The oracle work that remains is the corpus (pinned by commit in Task 21 per
`00-synthesis.md` §8) and the band (two engine runs to establish it before judging a third).
