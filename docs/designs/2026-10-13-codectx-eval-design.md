# codectx evaluation: the golden set and its runner

This document is the design of codectx's evaluation: a git-tracked golden set and one runner that score every answer the
product serves. The survey of other tools, papers and benchmarks behind each choice, the oracle tools used before the
first run, and the licence texts are in [research 30](../research/30-evaluation-golden-set.md). Contract references
point to the product documents in `docs/`.

## 1. What this is for

**Intended outcome.** A committed golden set plus one runner, both in the repository. The runner measures
whether every answer codectx gives is **accurate** (the right facts) and **clear** (each fact says how certain it is,
and the answer says what it could not do). It covers every capability the product serves, in every language where
the capability applies. Cases are built to be extremely hard.

**When it runs.** Only in the final test phase, after all code is in. It never runs during development or in CI.
The phase holds three full runs (section 5.6):
1. a measurement run;
2. after the fix round, the generalisation run: the first look at the held-out cases and at a fresh renaming of
   every case;
3. after the held-out findings are fixed, a regression run over every case, with another fresh renaming.

"Runs once" is read as "once the code is complete", not "one execution" (A17).

**Who reads the result.** The maintainers, to decide whether the product is ready, and the fix round, as a list
of findings.

**Success criteria.**
1. Every capability in section 4 has golden cases and one exact scoring rule.
2. Every case runs end to end: source → `codectx index` → the agent-facing answer. Nothing is checked on hand-built
   graphs.
3. Each answer is scored in two ways. **Contract**: what codectx's own documentation promises. **Truth**: what the
   language actually does.
4. The product cannot be tuned to the set: the overfitting guardrails (section 13) keep product code general.
5. The harness has no tests of its own, yet every score can be checked by hand from the report.
6. Tracked files obey the repository rules (section 14).

**Defaults.** Where the product's documents leave a choice open, the design takes a default, listed in
section 17 with its reason.

**Out of scope.**
- **Performance and memory proofs** stay in the separate live proofs. The report records wall time and peak RSS per
  project for information only; nothing is scored on them.
- **Install, toolchain lifecycle, SBOM and release artifacts** are product-boundary behaviour. `internal/e2e` and the
  release checks own them.
- **`tests` and `owns` relations.** No provider publishes them (`internal/model/facts.go:91,97`), so there is
  nothing to score.

## 2. The gap it closes

- **What exists today.**
  - The link goldens (`internal/provider/link/golden_*_test.go`, 117 cases) check stored link facts on small
    programs.
  - The lowering goldens (`internal/provider/treesitter/worker/lower_*_test.go`) check control and data dependence
    pairs per function.
  - `internal/graph/callanswer_test.go` with `testdata/dispatch.json` checks query-time answers on hand-written
    graphs.
  - `internal/index/delta` checks that a refresh's stored rows equal a full build's (`TestIncrementalLinkEqualsFullBuild`,
    `TestRandomEditSequencesEqualFullBuild`).
  - `internal/e2e` checks the process boundary on a tiny repository with the optional providers switched off.
  - A separate relevance benchmark, kept outside the repository, scores search and context on large public
    repositories. It never scores graph answers.
- **What none of them do.** None checks the end-to-end answer an agent receives: with every provider on, on
  cluttered multi-module code, against independently derived ground truth, across every capability.
- **ADR-0015 §7 asks for this.** It requires `may_call` precision and recall "measured per language against
  compiler-grade ground truth on the fixtures and published with the linker's documentation". The harness is where
  that measurement lives.

## 3. Approaches considered

| | Approach | Verdict |
|---|---|---|
| **A** | Self-written extreme-difficulty projects per language, committed. Spec-anchored expectations derived twice (the second blind) and cross-checked against compiler-grade and runtime oracles, in an uncommitted copy, before the first run. Bulk clutter (hub names, vendor copies, generated code) comes from a seeded, deterministic generator. | **Recommended** |
| B | Pinned slices of real repositories copied into the tree. | Rejected. One of the large public corpora is licensed under terms that do not permit excerpts in an Apache-2.0 repository, and Apache-2.0 or MIT excerpts carry notice duties (research 30, section 8). A slice also loses the cross-module context that makes real code hard. Scale realism on real code is the separate relevance benchmark's job (section 15). |
| C | Run other analyzers (language servers, precise indexers) as live oracles at evaluation time and diff against them. | Rejected. Research 24's six-server probe found the analyzers disagree with each other and with codectx's own semantics: some list `direct(d: Dog)` among `Animal.sound`'s callers, none marks a call through a base, and two list no through-base caller at all (research 30, section 3). The runs are heavy, nondeterministic and need network, and the scores would measure the analyzers' drift. |

Approach A makes truth a property of the committed data, not of tools at run time. The oracles still do their job:
they catch the authors' mistakes before the first run (section 5.4).

## 4. Capability inventory

These are what the product serves (`internal/mcpserver/registry.go`, 24 tools; `internal/cli/root.go`). The scored
surface is the MCP tool, because agents read the product through it; the CLI is used for `index`, `refresh`,
`status` and the parity slice (section 7.19).

| # | Capability | Surface scored | Languages |
|---|---|---|---|
| 1 | Structure: declarations, kinds, names, qualified names, ranges, nesting, exports, header grammar choice | `codectx_find_symbol` `document-symbols`, `codectx_symbol_info` | all nine |
| 2 | Symbol resolution: exact, qualified, prefix, id; ambiguity | `codectx_find_symbol` `resolve`/`definition`; graph-argument resolution | all nine |
| 3 | Search: five tiers, filters, order, paging | `codectx_search` | all nine |
| 4 | Repository map and metadata rows | `codectx_repo_overview`, `codectx_node_metadata` | all nine |
| 4b | Documents and file classification: Markdown `section` nodes, `documents` links, README to directory, `document`/`configuration`/`build_target` nodes, binary detection | callees and references with `relations: [documents, defines, contains]`, search | language-independent |
| 5 | Callers and callees with dispatch, through-base and may-dispatch marks, value sets | `codectx_callers`, `codectx_callees` | all nine |
| 6 | `implements`, `extends`, `overrides`, including external supertypes | callers/callees with `relations`, `codectx_references` `implements` | all nine (Go: method sets) |
| 7 | `imports`, open import sites, `may_import` | callees with `relations: [imports, may_import]` | all nine |
| 8 | Open call sites, reasons, `may_call` candidates, counts and notices | callers/callees | all nine |
| 9 | References and override families, precision classes, LSP overlay label | `codectx_references`, `codectx_find_symbol` `semantic_source: lsp` | all nine |
| 10 | Dependency path | `codectx_dependency_path` | all nine |
| 11 | Impact: entries, direction, rank, reasons, routes, package rollup | `codectx_impact` | all nine |
| 12 | Dependence: `data_flows_to` (all roles), `control_depends_on`, `reads`, `writes`, return flow | callers/callees with `relations`, `codectx_impact` | all nine |
| 13 | Manifests and lock files: `depends_on`, `configures`, `builds`, resolved versions, unresolved markers | callees and references with `relations`, node metadata | go, npm, cargo, python, maven, composer manifests |
| 14 | Routes, request inputs, entry points | callees from endpoints; `routes_to`, `defines`, `reads` | Go, Python, JS/TS, Java |
| 15 | Context packing: task → plan, required scope, budgets, exclusions, determinism | `codectx_context_plan` and its views | all nine |
| 16 | Context sessions: receipts, waivers, gate | the session tools | language-independent |
| 17 | Read source | `codectx_read_source`, `codectx source` | language-independent |
| 18 | Disclosure: capability rows, counts, notices, truncation, status | `codectx_index_status`, `QueryMeta` of every answer | all nine |
| 19 | Incremental refresh: refresh equals a fresh index | `codectx refresh`, then every case of the project | all nine |
| 20 | Determinism and adapter parity | metamorphic, no expected answer | all nine |

## 5. Building the golden set

### 5.1 Projects

- **One project is one repository root.** It is a small but real multi-file, multi-module tree that compiles or runs
  with its language's own toolchain, apart from files that are broken on purpose and say so in a comment.
- **Dependencies.** A project uses only the standard library and modules inside the project. Go uses `go.work` and
  local `replace`; `package.json` uses workspaces; `Cargo.toml` uses workspace members; `pom.xml` uses `<modules>`. Nothing is
  fetched. The Go linker reads external packages from the module cache, so a third-party `require` would make
  results depend on the host.
- **Framework imports.** Route projects import real framework paths in source, because detection is gated on those
  imports (`docs/providers-treesitter.md:404-410`). The existing route tests do the same.
  - **Stub APIs.** Each project carries a small stub of the framework API it calls, written from scratch: only the
    types, functions and annotations used. The stub sits where each toolchain resolves it offline:
    - Go: a `replace` directory inside the project;
    - Java: the annotation and class sources inside the module;
    - JavaScript and TypeScript: a workspace package of the framework's name;
    - Python: a top-level package.
  - **Why stubs.** Route projects then compile like every other project, and every precise provider stays `fresh`
    on them. A user's real route project also compiles.
  - **The real frameworks** are installed only in the uncommitted oracle copy, to dump each router's real table
    (section 5.4).
- **Package names in manifests and lock files are invented** (`@acme-eval/left-pad`, `acme-eval-codec`), and lock
  integrity values are synthetic. The lock is then a well-formed instance of its format. It names no real package,
  and nothing in it is resolvable from a registry.
- **Deliberate breakage lives in its own projects.** Each language has one `degraded` project. Its files are broken
  on purpose (a syntax error, an oversized file, a non-UTF-8 file, an import that resolves to nothing). Its case
  files declare, in `expect.capabilities`, the exact state and diagnostic code expected of every provider row the
  breakage touches. No other project contains a broken file, so no breakage can cost another case its compiler-grade
  facts.
- **Clutter is built into the trees:**
  - `vendor/` copies;
  - generated files carrying the Go "Code generated … DO NOT EDIT." header or an equivalent;
  - nested modules (a `go.mod` under `tools/`, a nested `package.json`, a Cargo workspace member);
  - files with the same name in many packages;
  - build-tag, `#[cfg]` and `#if` variants;
  - a `.h` header shared by C and C++ code.

### 5.2 Anchors in source, expectations beside the tree

- **Anchors.** A source line is tagged with a trailing comment `@tag`, in the comment syntax of its language. This
  matches the existing link goldens and the inline-marker style of the language servers' own test suites
  (research 30, section 2).
- **References.** A reference names a token on a tagged line as `token@tag` (`sound@s1`). `token#2@tag` means the
  second occurrence on that line. Expectations never contain byte offsets, so editing a comment moves nothing.
- **Expectations live in case files outside the project tree:** `cases/<language>/<project>.json`. If they sat
  inside the tree, codectx would index them, and they would change the search, structure and manifest answers they
  score.
- **Why both anchors and case files.** Anchors alone (the annotated-source style of call-graph suites) cannot
  express search, impact, context or refresh expectations. Separate expected-answer files alone make positions
  brittle (research 30, section 1).
- **Authoring rules.**
  - One call per golden line unless the case is about calls on the same line.
  - No tag equals an identifier any search case queries; the runner checks this at startup.
  - Every source file is gofmt-clean and has no trailing whitespace. Go files are gofmt-clean because CI's `gofmt -l .`
    does not skip `testdata`; `git diff --check` rules out trailing whitespace in any language.

### 5.3 The case schema (one schema for every capability)

```json
{
  "project": "go/hierarchy",
  "split": "open",
  "roots": ["puppy", "animal", "walk", "speak", "direct"],
  "cases": [{
    "id": "go/hierarchy/callers-puppy-sound",
    "capability": "callers",
    "tool": "codectx_callers",
    "args": {"start": ["sound@puppy"], "relations": []},
    "tier": 3,
    "features": ["override-chain", "cross-module", "interface-dispatch"],
    "anchor": "Go spec, Method sets and Selectors: https://go.dev/ref/spec#Method_sets",
    "derivation": "two_blind",
    "expect": {
      "rows": [
        {"from": "walk@walk", "rel": "calls", "to": "sound@puppy",
         "sites": [{"at": "sound@w1", "dispatch": "exact"}]},
        {"from": "speak@speak", "rel": "calls", "to": "Sound@animal",
         "mark": {"kind": "through_base", "base": "Sound@animal", "base_overrides": 3},
         "sites": [{"at": "Sound@s1", "dispatch": "virtual"}]},
        {"from": "Sound@puppy", "rel": "overrides", "to": "Sound@dog"}
      ],
      "counts": {"direct": 1, "through_base": 1, "may_dispatch": 0, "may_call": 0, "open_sites": 0},
      "notices": []
    },
    "prohibited": [
      {"from": "direct@direct", "rel": "calls", "to": "sound@puppy",
       "why": "direct(d Dog) can only run Dog.Sound"}
    ],
    "truth": {"same": true},
    "fails": "listing an override's direct callers under a deeper override; dropping the through-base climb past depth 1"
  }]
}
```

- **`split`** is `open` or `held_out`. **`roots`** lists the invented roots the renamed copy replaces (5.6). This
  example uses plain English names for readability; real cases use invented roots.
- **`args`.** Arguments are passed to the tool as written, except that `token@tag` references are first resolved to
  node ids by location.
- **`expect` is the complete answer** (closed world). A returned row that is not listed is an extra row. Rows are a
  set unless the capability's contract defines an order (search, impact ranking, `may_call` closeness, document
  symbols); then `expect.order` gives the rule (section 6.4).
- **`prohibited`** lists rows that must never appear: the prohibited-targets pattern of call-graph suites. Without
  it, an over-approximation passes silently (research 30, section 1).
- **`truth`** is `{"same": true}`, or the rows where runtime truth differs from the contract (section 6.3).
- **`fails`** names the wrong answers the case catches, as the existing goldens do.
- **`derivation`** is `two_blind` or `adjudicated`. An adjudicated case carries `note`: the disagreement and the
  spec text that settled it, in plain words, with no tool names.
- **Large answers.** Hub answers with thousands of rows give `expect.count` plus `expect.first` (an ordered head) and
  `expect.order`. Rows that come from the generator are listed by the generator, never by hand.

### 5.4 How expected answers are produced

Expectations are derived from the language specification and codectx's documented contract. They are never derived
from codectx output. No author runs codectx on a golden project before run 1.

The runner itself is built against throwaway projects copied into scratch from existing unit-test fixtures (the link
goldens' programs). Those are never golden projects and never committed under `internal/eval/testdata`. Run 1 is the
first time codectx sees a golden project.

1. **First derivation.** The author writes the project and its expectations, citing the anchor.
2. **Blind second derivation.** A second author derives every expectation again from the sources, the anchors and
   the contract documents alone, without seeing the first. The two must agree on every row and mark. The repository
   already works this way: Section 30 requires goldens "derived twice, the second blind".
3. **Oracle cross-check, before run 1.** An uncommitted copy of every project is checked by tools that never appear
   in tracked code. Research 30, section 4 names each tool and how it is driven.

   | Language | Static oracle (binding and hierarchy) | Second static oracle | Runtime oracle (what actually runs) |
   |---|---|---|---|
   | each of the nine | the language's reference compiler or type checker: each call site's resolved target and each type's supertypes and overrides, plus the language's own lower-bound and upper-bound call graphs where it has them | an independent analyzer of the same language, where one exists | a function-entry trace, inserted into the private copy, that records each call's callee and call-site position |

   For the other capabilities:
   - **Dependence.** The compilers' own intermediate forms give def-use and the control-flow graph for
     post-dominators where they expose them. Elsewhere, the two blind derivations follow the textbook definitions
     (data dependence on reaching definitions; control dependence by post-dominance, after Ferrante, Ottenstein and
     Warren).
   - **Routes.** Each application's runtime router table, dumped in the uncommitted copy with the real framework
     installed.
   - **Manifests and locks.** The format specifications. Each lock is also read by its own package manager, offline,
     where that is possible.
   - **Structure.** The compilers' syntax trees and the language servers' document symbols, for names, nesting and
     name ranges.

4. **Agreement rule.** A row is committed when the two derivations agree **and** no oracle contradicts it. An oracle can
   contradict only within its competence. A static oracle speaks to binding and hierarchy. A runtime trace speaks to
   what runs: every traced edge must appear in `expect` or `truth`, and a decoy that is never traced confirms a
   negative. Any disagreement is adjudicated against the language specification by a third person, and the case
   becomes `adjudicated` with its `note`.
5. **Compile-cleanliness.** Every file outside the `degraded` projects builds with its own compiler at the oracle
   step. This repeats the
   existing rule that every golden source is compiler-checked.
6. **What is kept.** The oracle drivers and their raw outputs are never committed. Research 30 records the tools,
   their exact versions and argv, and the agreement statistics: per language, rows checked, disagreements, and how
   each class was settled.

### 5.5 The generator (bulk clutter)

- **What it is.** A seeded, deterministic generator in the runner (`generate.go`). Committed recipes
  (`generate/<name>.json`) tell it what to emit:
  - a hub name declared about 7,500 times across about 1,500 files per language family (ADR-0015 cites 7,188
    definitions of one name);
  - vendor copies of a project package;
  - generated-code directories;
  - deep nested-module trees.
- **Output.** The runner materialises the output into scratch, never into the tree. The same recipe and seed give
  the same bytes, so a change shows only as a change to a committed recipe. A recipe also emits its own expectations: exact counts, and the ordered head under
  the documented closeness order.

### 5.6 Held-out cases, renamed copies and the three runs

The golden set is tracked like any other file, and git is its record. The runner never edits an expected answer.
There is no mode that writes product output into expectations, because that is tuning to the set.

- **Held-out cases.** About 30% of the hand-written cases, stratified by language × capability × tier, are written
  by authors who take no part in the fix rounds. They stay outside the repository until after run 2, in the same
  layout, and the runner reads them from `--held-out <dir>`. Before then, the person who runs the harness gives the
  fix round the open cases' full findings and only the held-out aggregates (counts per capability × language),
  never a held-out case id or row. After run 2 they are committed under `internal/eval/testdata/` like the rest.
- **Renamed copies.** Every authored name in a project is built from invented roots (`zorbel`, `quintac`) that occur
  nowhere else and are not words of any language, library or framework. Each case file lists its roots. For each
  run the runner draws a fresh seed, maps every root to a new invented root of the same length, and substitutes it
  in every file's contents, in file and directory names, in the refresh edit steps and in the case file, keeping
  each occurrence's letter case (`zorbel`, `Zorbel`, `ZORBEL`) so exported and unexported names keep their meaning. It also inserts a block of comment lines at the top of every
  source file, after any shebang or encoding line. Both changes preserve meaning in all nine languages, so every
  renamed expectation follows from the original by the same substitution, and the renamed copy is scored like the
  original. Names a language, library or framework gives meaning to (`main`, `__init__`, `Default`, `toString`,
  manifest keys) are never roots.
  - **Scored on the copy.** Every case, except the byte totals of the repository map and the byte floors of
    `CTX_MINIMUM_BUDGET`, which the comment block legitimately changes. Order rules are constraints (6.4), so a
    tie broken by name still holds under new names.
  - **The flag.** A case that is `exact` on the original and not on the copy is flagged `name_dependent` (S1): the
    product answered from something other than the code's meaning.
  - **Why it works.** A fix keyed on a golden name, file or layout fails a copy whose names it has never seen, and
    the seed changes every run (research 30, section 10).
- **The three runs.**
  1. **Run 1** scores the open cases, the held-out cases and a renamed copy. The fix round works from the open
     findings only.
  2. **Run 2** is the generalisation run. The product was built without seeing the held-out cases or this run's
     names, so its scores on them are an honest estimate of how it does on unseen code. Its two gaps are the
     headline numbers: open against held-out, and original against renamed. A gap of more than a few points means
     the fixes fit the set rather than the language. The held-out cases are then committed, and their findings are
     fixed.
  3. **Run 3** is a regression run: every known case must pass. It cannot measure generalisation on the held-out
     cases, which the fixes have now seen, so its unseen part is another fresh renaming.

  More looks at the held-out cases before run 2 would wear out their independence (research 30, section 10), so
  the fix round gets aggregates only and there is no extra run in between.
- **No overlap with development fixtures.** No golden source file may be byte-identical to a fixture the unit tests
  use, and the runner checks this at startup. The cases are new programs, so a fix that passes the existing goldens
  says nothing about them.

## 6. The scoring rule all capabilities share

### 6.1 Collecting and normalising an answer

- **Collecting.** The runner follows every cursor to the end, so the answer is complete under defaults, as an agent
  receives it. It keeps every page.
- **Normalising nodes.** Each returned node is mapped to a reference through the answer's own locations:

  | Node | Reference |
  |---|---|
  | declaration | `token@tag`, its `name_bytes` |
  | module | `module:<path>` |
  | external | `external:<qualified_name>` |
  | open site | `open:token@tag` |
  | endpoint | `endpoint:<METHOD route>@<path>` |
  | variable | `var:token@tag` |
  | return node | `return:<callable ref>` |
  | manifest or dependency node | its qualified name (`npm:@acme-eval/x`) |

  A node with no tag maps to `unanchored:<path>:<start>-<end>`, which can only ever be an extra row.
- **Never compared.** `generation_id`, `snapshot_id`, `analysis_key`, `repository_id`, cursors and answer tokens,
  `evidence_id`, `relation_id`, `node_id`, run and ledger fields, and timestamps.
  - The repository id is computed from the root path (`internal/index/coordinator.go:260`). Normalising by location
    is what makes answers comparable across roots.

### 6.2 Matching rows and the flags a case can carry

- **Matching.** A returned row matches an expected row when its endpoints' references and relation kind are equal.
- **Sites.** Call sites match by file, and by the occurrence's byte range containing the referenced callee token.
- **A case is `exact` only when it carries no flag.** The flags:

| Flag | Meaning | Severity |
|---|---|---|
| `overclaim` | A row is stated with more certainty than the contract allows. This covers a `calls` row where `expect` has `may_call`, which ADR-0015 forbids **even when the target is right**; an unmarked row where `expect` has a mark; and precision `compiler` on a wrong row. | **S1** |
| `prohibited` | A row from `prohibited` appeared. | **S1** |
| `false_row` | A precise row that is neither in `expect` nor in `truth`. | **S1** |
| `silent_miss` | An expected row is absent and nothing discloses it: no open site naming that call site, no notice, no capability row that is not `fresh`. | **S2** |
| `false_complete` | An answer has a `silent_miss` while every capability row covering its scope says `fresh`. | **S2** (counted beside the miss) |
| `wrong_mark` | The row matched, but its mark differs: mark kind, base, `base_overrides`, `set_size`, or per-site `dispatch`. | **S3** |
| `disclosure` | `counts`, a required notice, a capability state or count, a reason code, or an open site's `reason`/`shape`/`arguments` differs from `expect`. | **S3** |
| `underclaim` | A row carries a mark where `expect` has none. `value_set` is a mark like the others: an expected `value_set` row is not exact. | **S3** |
| `extra_possible` | An extra row is marked possible, or is a `may_call` beyond the contract's candidate rule. | **S4** |
| `order` | A contract order rule (section 6.4) is violated. | **S4** |
| `disclosed_miss` | An expected row is absent, but an open site or notice discloses the gap with the right reason. | **S5** |
| `error` | The tool failed, or a different error code came back than `expect.error`. | **S1** |
| `name_dependent` | The case is `exact` on the original and not on the renamed copy (5.6). | **S1** |

- **Headline verdict.** A case's headline verdict is its most severe flag. The report lists every flag, never just
  the worst.
- **Why this order.**
  - A certain-looking wrong fact (S1) is the worst outcome: an agent acts on it.
  - A silent gap (S2) is next: an agent concludes "nothing there".
  - A gap that is disclosed (S5) is the product working as designed when analysis cannot decide.
- **Verdict styles.** A call-graph suite's "unsound" verdict is `silent_miss` here, and its "imprecise" verdict is
  `extra_possible`, `prohibited` or `false_row` (research 30, section 1).

### 6.3 Contract lens and truth lens

- **Contract lens.** Every case is scored against `expect`, the documented contract. This is the lens whose findings
  must be fixed.
- **Truth lens.** Where the language's runtime behaviour differs from the contract, `truth` lists the differences:
  - `truth.runs_unlisted`: rows that run but the contract does not publish (a decorator's wrapper, an operator
    method), each with `documented: true|false`;
  - `truth.listed_never_runs`: contract rows that never run, such as a class-hierarchy override no code
    constructs.
- **Reported apart, never mixed in.**
  - **Truth precision and recall** over what runs.
  - **Documented limitations**: rows that run but are absent, where the contract says so.
  - **Silent divergences**: rows that run but are absent, where nothing says so. These are findings against the
    contract itself, each settled before run 1.
  - **Over-approximation**: possible rows that never run. This is the class-hierarchy price ADR-0015 accepts.
- **Truth figures are bounds.** "What runs" comes from the recorded runtime trace of each project's driver plus the
  static oracles. Precision measured against a trace is a lower bound on true precision, and recall against it is
  bounded, not exact (research 30, section 1).
  The report prints them as bounds.

### 6.4 Order rules

Each rule is checked as a constraint, never as one fixed permutation, so ties the contract leaves open are not
scored:

| Capability | Order rule |
|---|---|
| Search | Tier order; within an exact tier, the content key; within the lexical tier, graded relevance (6.5) |
| `may_call` | Closeness order: same file, same directory or package, imported file, same unit, elsewhere; then fewer compatible definitions; then a stable key (ADR-0015 §3) |
| Impact | Every call, override or structural entry before every dependence-only entry; `score_micros` descending, then `depth`, then `node_id` |
| Callers and callees | Precise rows before possible rows |
| Document symbols | Document order |

### 6.5 Metrics (all reported per capability × relation kind × language × tier × split)

| Metric | Definition |
|---|---|
| Case exact rate | `exact` cases ÷ cases |
| Row precision P | TP ÷ (TP + FP). TP is the expected rows matched with any mark. FP is the returned rows that are neither expected nor listed in `truth.runs_unlisted`, prohibited rows included. A returned `truth.runs_unlisted` row is true but beyond the contract: it is counted apart as "beyond contract", never as FP. |
| Row recall R | TP ÷ (TP + FN). FN is the expected rows absent. |
| F1 | 2PR ÷ (P + R) |
| Mark accuracy | matched rows with every mark field right ÷ TP |
| Certainty error | (`overclaim` rows + `underclaim` rows) ÷ TP |
| Disclosure rate | `disclosed_miss` rows ÷ FN. Silent misses are FN minus `disclosed_miss`. |
| Per-site target recall (callees) | for each call site, the expected targets returned ÷ the expected targets; then the mean over sites. This is the navigation-client metric of the call-graph studies (research 30, section 1) |
| Abstention-aware score | for each expected row, +1 if returned with the right mark, 0 if missing but disclosed (`disclosed_miss`), −1 if missing silently; and −1 for each `false_row`, `prohibited` or `overclaim` row. Divided by the number of expected rows. This is the "wrong costs t/(1−t), I-don't-know costs 0" rule at t = ½ (research 30, section 7), and it rewards disclosure over guessing. Reported beside P and R, never instead of them |
| Precision by class | P over rows whose evidence carries each precision class (`compiler`, `language_server`, `static_analysis`, `syntax`, `heuristic`). A `compiler` row is a claim of certainty, so its P is reported first. |
| `may_call` candidate recall | open sites whose true target is among the candidates ÷ open sites with a true target in the project |
| `may_call` MRR | mean of 1/rank of the true target, 0 when it is absent; ranks follow the served order |
| Search | Success@1, Success@10, MRR@10, nDCG@10 over graded judgments (2 = the answer, 1 = acceptable, 0 = not relevant). Judgments are exhaustive: every document of the project that contains a query token or token part is judged, so Judged@10 (research 30, section 5) must be 100%. An unjudged hit still counts as 0 and is listed as an integrity note. The exact tiers are set-and-order equality. |
| Context | land rate (every evidence group lands in a slice of the packed plan) ÷ cases; group recall; required-decoy rate (a prohibited file marked `required_*`); floor exactness for `CTX_MINIMUM_BUDGET` |
| Refresh | divergent answers after refresh ÷ answers compared; must be 0 |
| Truth lens | truth P, truth R, documented limitations, silent divergences, over-approximation count |

Severity counts S1–S5 are reported beside every row of every table.

## 7. Per-capability cases and scoring

Below, "all nine" means C, C++, Go, Java, JavaScript, Python, Rust, TypeScript and TSX. The per-language extreme
constructs are listed in section 8; this section gives what each capability's cases contain and its exact rule.

### 7.1 Structure

- **Cases.** One case per file asks for `document-symbols`. `expect` lists every declaration:
  - kind;
  - name;
  - `qualified_name` as the contract spells it (package or ancestors joined by `::` for C, C++ and Rust, `.`
    otherwise);
  - name token reference;
  - parent;
  - the full range's first and last token references.

  Each case also covers the `exports` set and, for `.h` files, the kept grammar with `header_reason`.
- **Extreme constructs.**
  - Overload families (Java, C++, TS).
  - Declarations in both `#if` arms, in `#[cfg]` variants, in `if`/`else` arms (Python) and in `try` arms.
  - Rust `macro_rules!` item-position bodies.
  - Go generic methods and `new(expr)`.
  - C/C++ macro-prefixed declarations and prototypes beside definitions.
  - Java anonymous classes (`new T` nodes), enum constant bodies and records.
  - JS/TS function-valued properties (`a.b = () => …`, `{k: () => …}`).
  - Python `__all__`, both static and computed; the computed one falls back with a capability note.
  - TS declaration merging and namespaces; JSX components.
  - Exact duplicates (a `def f` written twice), Go `_` and Rust `const _` (which declare nothing), and names at the
    qualified-name storage ceiling.
  - In the `degraded` project: a deliberately broken file, which must be `partial`/`CTX_COVERAGE_INCOMPLETE` and
    leave every other file `fresh`; and a non-UTF-8 file (`unavailable`/`CTX_PROVIDER_UNAVAILABLE`).
- **Rule.** Declarations match by name token. P and R are computed over declarations, plus per-field accuracy for
  kind, qualified name (exact string) and parent.
  - **Range.** The scored rule is containment: the range covers the first and last referenced tokens and no sibling
    declaration. Exact byte equality is reported for information only, because languages do not define where a
    declaration starts (doc comments, decorators, annotations).
  - **Header grammar.** `header_first`, `header_kept` and `header_reason` are exact.

### 7.2 Symbol resolution

The product has **two contracts**, and cases cover both:

1. **`codectx_find_symbol` `resolve`** serves the tiers id → exact qualified name → qualified-name byte prefix →
   exact name, case-sensitive, and returns **every** candidate, paged. It never errors on ambiguity. `definition`
   needs a file and a range; `document-symbols` needs a file.
2. **A graph argument** (`codectx_callers` with a name) resolves exact `name` first, then exact `qualified_name`. A
   miss and an ambiguity are both `CTX_ARGUMENT_INVALID`; the ambiguous case carries `candidates` with the exact
   count and never picks one.

- **Extreme cases.**
  - A name declared in 40 packages, a hub name under the generator, and names differing only in case.
  - A qualified name that is a byte prefix of another (`a.b` against `a.bar`).
  - A 64-hex string that is not a visible id, so resolution falls through.
  - A return node, which is never a symbol.
  - `workspace-symbols` over the whole generation, paged to the end with no duplicates.
  - The LSP path (`semantic_source: lsp`): answers must carry the overlay label, and must never return index facts as
    if they came from the overlay.
- **Rule.** Candidate set equality, tier per candidate, exact error code and `candidates` count. Plus
  `codectx_symbol_info`'s documented behaviour: evidence for the first candidate only.

### 7.3 Search

- **Cases.**
  - **Exact path.** Every node of the file.
  - **Exact qualified, prefix and exact name tiers.** Set and order equality.
  - **Lexical.** Graded judgments written by the authors on the golden projects.
  - **Filters.** `kinds`, `languages` and `paths` (MCP-only), including a `.h` file whose kept grammar decides its
    language.
  - **Tokens.** camelCase parts, quoted phrases, Unicode case folding and diacritics, and a punctuation-only query.
  - **Return nodes.** Excluded unless `kinds` names `return`.
- **Paging cases (metamorphic).** The concatenated pages at `limit` 1, 7 and the maximum must be one sequence with no
  duplicates. Because the lexical tier is ranked, this is checked over the head the pages serve: never all matches
  (a ranked search serves its best results).
- **Rule.**
  - Exact tiers: equality.
  - Lexical: the metrics in 6.5, with 0 for unjudged hits. A hit judged 2 must also land within the tier the contract
    places it in.
  - Paging: any duplicate or gap is a `disclosure` flag.

### 7.4 Repository map, metadata rows, documents and file classification

- **Cases.** Container trees with nested modules, vendor and generated directories, and canonical names stripped of
  provider quoting.
  - Totals are **direct** containment, not transitive.
  - An over-8-KiB metadata field must come back as `<field>_rows: n`, and its rows must concatenate to the field.
- **Rule.** Exact equality of container set, parent, `file_count`, `symbol_count` and `source_bytes`. Totals are
  computed by construction.
- **Documents and classification cases.**
  - Markdown heading nesting, giving `section` nodes.
  - Links to workspace paths: relative, with `#anchor`, to a directory, broken (publishes nothing), outside the
    workspace (publishes nothing).
  - A README's `documents` edge to its directory.
  - Classified paths (Dockerfile, compose, CI configs, Terraform, Helm, OpenAPI, CMake, Bazel, Make), which give one
    node and nothing else.
  - A binary file: over 30% non-text in the first 8,000 bytes, or a run of 4 NULs there, makes search
    `unavailable` with `CTX_BINARY_CONTENT`. Scattered NULs alone do not.
  - A file with some non-UTF-8 chunks makes search `partial`.
  - **Rule.** Node and edge set equality, precision class `heuristic`, and exact capability states
    (`docs/providers-filesystem.md`).

### 7.5 Callers and callees

- **Cases.** In every language:
  - direct, imported and scoped calls;
  - calls through a declared type (one `calls` to the declared or inherited method, `virtual`);
  - `super`, qualified, constructed-object, static, final and private calls, which are `exact`;
  - factories: a call on a factory's result, typed by the declared return type (`make().sound()` with return type
    `Animal` is `virtual` to `Animal.sound`; a C++ class returned by value is `exact`), and registry factories that
    pick a class by key (a value-flow set, or open when the key is computed);
  - value-flow sets (`value_set`, "1 of n");
  - function-pointer and field sets: each row carries a `value_set` mark with `set_size` n (the product marks every
    `calls` row of a set of 2 or more, `internal/graph/callmarks.go:103-107`), and each site's `dispatch` is `exact`;
  - three-level override chains across modules, where `callers` of the deepest override lists the through-base
    callers of every ancestor, each marked with its base and `base_overrides`;
  - `callees` of a virtual caller, which lists every override at any depth as `may_dispatch`;
  - the negative of the through-base rule: `callers(Animal.sound)` must never list `direct(d: Dog)`.
- **Rule.** 6.2 and 6.5. Per-site `dispatch` is part of the mark.

### 7.6 `implements`, `extends`, `overrides`

- **Cases.**
  - Declared supertypes and their kinds.
  - Go satisfaction by method set across modules, including the negative where same-depth ambiguity removes a method.
  - External supertypes: a node of kind `type` with detail `supertype_class` or `supertype_interface`.
  - Marked external overrides: Java `@Override`, C++ `override`/`final`, TS `override`, Rust trait impl items, Python
    `typing.override`.
  - `supertype_candidate` with "at least one of n".
  - Unmarked methods of a class with an external supertype publish nothing, and the callers answer must say so.
  - Go conversion proofs `var _ I = (*T)(nil)`.
- **Rule.** Row equality including `detail` and `set_size`. Edges deeper than the direct supertype are `prohibited`
  (`overrides` is direct only).

### 7.7 Imports and possible imports

- **Cases.** Every resolution path the contract names:
  - **ECMAScript.** Specifiers through `exports`/`imports` conditions, `paths`, `baseUrl`, project references,
    workspaces by name, `require` against `import` conditions, `export *` conflicts.
  - **Python.** `import a.b` and `from a import b`, resolved to the declaring file, then the submodule, then the module.
  - **C and C++.** Quoted `#include`: beside the including file, then a component-boundary suffix match with
    `set_size`.
- **Disclosure cases.**
  - Dynamic imports (`require("./x/" + n)`, `` import(`./p/${n}.js`) ``, `importlib.import_module(f"plugins.{n}")`,
    `#include MACRO`) must be open import sites carrying their fragments, with `may_import` candidates listed only
    when the request names `may_import`.
  - An unbuilt workspace `exports` target counts in `open_import_sites`.
- **Rule.** Row equality, plus exact `fragments` and the `may_import` candidate set.

### 7.8 Open call sites and `may_call`

- **Cases.** Each reason: `receiver_untyped`, `value_untracked`, `global_unbound` and `computed`. Each shape: `bare`,
  `member`, `value` and `construct`. Plus argument counts.
  - **Candidates.** Kind compatibility by shape, the same language family (C with C++, JS with TS), and arity in C,
    C++, Go, Java and Rust.
  - **The hub case.** The generator's ~7,500 definitions: an exact `candidates` count and the closeness-ordered head.
  - **Never a bare zero.** With zero callers, the notice must quote the linker's site counts.
  - **`may_refer_to`, when a request names it.** Every in-file candidate of a name declared more than once in the
    file (overloads, shadowing), kept uncapped beside the structural `calls` to the ambiguous callee reference, and
    the resolver's ambiguous alias matches (`docs/providers-treesitter.md:163, 245-247`). Scored by set equality: one
    candidate missing is a `silent_miss`, and any cut is a finding.
- **Rule.**
  - A name-only binding returned as `calls` is S1 `overclaim`.
  - The candidate set must equal the contract's rule over the project, and the head must satisfy the closeness order.
  - Candidate recall and MRR are reported per language, which is ADR-0015 §7's measurement.

### 7.8.1 Calls whose target depends on run-time values (A12)

Code-intelligence systems and the call-graph literature agree on one shape (research 30, section 11):
- **Three kinds of answer, never mixed.**
  1. One target the compiler would pick: an exact row.
  2. A closed set the code or the type system fixes: the declared target marked `virtual` with its overrides, or
     the set's rows with `set_size`. Every member is listed and marked possible.
  3. Only a name: an open site with its reason, plus ranked `may_call` candidates and their exact total.

  Systems label each answer by where it came from, and they keep closed sets apart from name matches. Calling a
  closed set name-only understates it, and calling a name match a set overstates it.
- **No name means no candidates.** For a computed callee every system examined returns nothing. A tool for IDE
  navigation was found to do better with no target than with many false ones, and irrelevant context measurably
  hurts language models.
- **A named reason, not a number.** Named failure causes help a reader more than raw confidence scores, and the
  soundness literature asks for each unsound choice to be named. codectx's `reason` and precision class already do
  this.
- **Precise first, candidates after, with an exact total.** People tolerate about 10–20% false positives before they
  stop trusting a tool, and agents do worse with long unranked lists. ADR-0015's order and its count with a ranked
  first page match this.

The expected answer for each construct:

| Construct | Expected answer | Kind |
|---|---|---|
| JS `f.call(o, …)` | `calls` to what `f` names, as if `f(…)` were written with receiver `o`; never a call to the built-in `call`. When `f` is not resolved, an open site for `f` (`value_untracked`) with candidates named like `f` | 1, else 3 |
| JS `f.apply(o, args)` | as `f.call`, except that candidates are not filtered by argument count, because the arguments arrive as one array | 1, else 3 |
| JS `const g = f.bind(o); g()` | `calls` to `f`'s target at `g()` when the bound function reaches `g` through local flow; the bound arguments count toward the arity check. Otherwise an open site (`value_untracked`) | 1, else 3 |
| Python `getattr(obj, "name")()`, constant name | the same answer as `obj.name()`: bound when `obj`'s type is known, else an open member site (`receiver_untyped`) with candidates named `name` | 1 or 2, else 3 |
| Python `getattr(obj, expr)()`, computed name | an open site, `computed`, no candidates | none |
| C++ ADL, `swap(a, b)` with `a` of type `ns::T` | exact `calls` to `ns::swap`: the compiler's lookup picks one function | 1 |
| C++ ADL inside a template, argument types depending on `T` | an open site with candidates named `swap`. Recommended contract addition: a reason `dependent` for a name whose meaning waits on a type parameter (also Rust and TS calls on an unbounded generic); until it exists the case expects `global_unbound` | 3 |
| C function-like macro `#define LOG(x) log_impl(__FILE__, x)`; `LOG(msg)` | exact `calls` from the enclosing function to the macro `LOG`, as the product binds today, plus `calls` from `LOG` to `log_impl`, so callers, paths and impact reach through the macro. A further macro inside the replacement chains the same way | 1 |
| C11 `_Generic((x), int: iabs, double: dabs)(x)` | exact `calls` to the selected function when `x`'s declared type is known; otherwise one row per listed function with `value_set` and `set_size` n, never `may_call`. Inside a type-generic macro, plus the macro chain above | 1, else 2 |
| Rust `Default::default()` or `T::default()`, concrete type inferred | exact `calls` to that type's `impl Default` `default` | 1 |
| Rust `T::default()` with `T: Default` generic | `calls` to the trait method, `virtual`, with the project's impls as `may_dispatch` | 2 |
| Rust `w.m()` through a custom `Deref` | `calls` to `m` on the `Deref` target type. The compiler's implicit `deref` call is not call syntax, so it is recorded in the truth lens (A9) | 1 |
| JS `Object.assign(Foo.prototype, Mixin)` with `Mixin` a literal object or class in the code; `new Foo().mixed()` | exact `calls` to `Mixin.mixed`: copying the properties is a fixed step. With a source that is not statically known, an open site (`receiver_untyped`) whose candidates rank the mixin's member first by closeness | 1, else 3 |
| TS `function M<T extends Ctor>(B: T) { return class extends B { m() {} } }`; `class C extends M(Base)` | `extends` from `C` to the class expression; `c.m()` to `m` in the class expression; `c.baseMethod()` to `Base.baseMethod` (`virtual`), because the checker types `M(Base)` structurally. Inside `M`, `super.x()` binds to the constraint's member when the constraint declares it, else an open site | 1 or 2, else 3 |

### 7.9 References

- **Cases.**
  - Occurrences of a method family both ways (references serve rename, so the whole family).
  - Type references.
  - C and C++ visibility macros (`references` with `visibility_*` detail, one per `#if` arm with `set_size`).
  - `may_call` sites as `heuristic` occurrences after the precise ones.
  - The `implements` and `type-definition` operations.
  - LSP overlay answers (labelled, `language_server` precision). This covers `semantic_source: lsp` on symbol
    and reference requests, scored on the same expectations and checked for the `overlay` label.
    - The model's comment also names call-hierarchy routing (`internal/model/query.go:190`), but the MCP
      `codectx_callers`/`codectx_callees` input has no `semantic_source` field (`internal/mcpserver/handlers.go:104`).
      No overlay call-hierarchy case is written until that is settled (A23).
- **Rule.**
  - Occurrence set equality by location.
  - Relation-count and occurrence-count equality (one relation, two occurrences).
  - Order by canonical relation id is checked only as "pages concatenate to the single-shot answer".
  - Precision-class correctness.

### 7.10 Dependency path

- **Cases.**
  - Chosen pairs whose cheapest route is known from the integer costs (`internal/graph/cost.go:19-58`): `calls`,
    `implements`, `extends`, `overrides` and `routes_to` 1; `imports`, `exports`, `depends_on`, `references` and
    `data_flows_to` 2; `control_depends_on`, `reads` and `writes` 3; `may_call` 7.
  - Several equal-cost routes, so the whole set is checked.
  - A pair reachable only `incoming`, so `outgoing` must say "no route that way", not "no path".
  - A pair joined only through `may_call`, so routes are marked `possible` with per-route hop counts.
- **Rule.** Exact cost, exact route set, exact `route_count`, and the `possible` mark.

### 7.11 Impact

- **Cases.**
  - The changed symbol's callers: through-base, overriding methods of a base, endpoints whose handler changed.
  - Dependence-only entries, which rank after every call-reached entry.
  - Realizability: a value entering a callee through one site must not leave through another site's result.
  - The package rollup's exact `pair_count` and `evidence_count`.
  - The reasons and routes views read with the answer token.
- **Rule.**
  - Entry set P and R, and direction accuracy.
  - Order constraints (6.4).
  - Each entry's reasons must include the expected relation kinds.
  - Rollup equality.
  - Views must concatenate.
  - Realizability violations are `prohibited` rows.

### 7.12 Dependence

- **What the contract says.** Endpoints are variables (tagged on their declaring identifier) and return nodes;
  statements are evidence ranges only.
- **Cases per language.** Every construct the lowerings name:
  - valued switch and match, short-circuit operators in value position, embedded assignments;
  - loops and iterables evaluated once, labelled break and continue, `goto`, switch fallthrough;
  - try/catch/finally (Java, JS, Python, C `__try`), Python `with` as a finally around `__exit__`, comprehensions as
    implicit functions;
  - Go `defer`/`go`/`select`, type switches;
  - Rust `?`, let-else, tail expressions, closures as separate callables;
  - may-definitions through `&`, `&mut` and non-const references;
  - generators and async functions.
- **Cross-file roles.**
  - `argument`: positional, keyword, `*args` and spread, variadic, defaulted parameters that receive nothing, a
    receiver passed first.
  - `summary:<pos>`, `return`, `return f()` into the caller's return node.
  - `g(f())` and `f().m()`, which have no return flow.
- **Negative cases for what is not modelled.** Fields, globals, points-to and Rust panics produce **no** edge, and
  the disclosure is checked.
- **Rule.** Row equality per relation kind and per role detail. The four per-file capability rows must be exact,
  including `failed_functions` and `unresolved_jumps`.

### 7.13 Manifests and lock files

- **Cases.** One polyglot project covering every manifest and lock format `docs/providers-filesystem.md` lists, each
  with its hardest legal shapes:
  - Go module and workspace files with `replace`, `exclude` and `use`;
  - `package.json` workspaces (globs stored unexpanded), with every listed lock format, including both forms of the
    format that has a classic and a YAML variant;
  - `Cargo.toml`/`Cargo.lock` with a virtual workspace, `{workspace = true}` (inherited, requirement unresolved) and
    renamed dependencies;
  - `pyproject.toml` with `dynamic` fields;
  - requirements files with `-r`/`-c` includes, a cycle, constraint lines, and unresolved lines of every reason;
  - every listed Python lock format;
  - `pom.xml` with a parent, `<modules>` (`builds`), `<dependencyManagement>` (`managed`) and `${prop}` (unresolved);
  - the listed PHP manifest and the listed Java lock file.
- **Version cases.**
  - A package locked at several versions, which gets one occurrence per version and `set_size`, never "resolved by
    preference".
  - A dependency listed under two kinds, which keeps both.
- **Disclosure cases.**
  - The formats the document says are not read must publish nothing.
  - An over-size file must be `unavailable`.
- **Rule.** Tuple equality on (from, to, kind, requirement, `resolved_version`, `integrity`, `set_size`, `source`,
  `group`), plus unresolved markers and capability states.

### 7.14 Routes, request inputs and entry points

- **Cases.** One project per routing family the product detects (`docs/providers-treesitter.md`, Framework routes;
  `docs/providers-link.md`, Route prefixes across files), in Go, Python, JavaScript/TypeScript and Java. The Go
  cases include the standard library's method-and-path patterns.
- **Route shapes.**
  - Prefixes across files and sub-applications: mounted sub-applications, routers imported across files, routers
    passed as parameters or returned.
  - A settings-chained URL configuration.
  - A route reached by two mounts (two endpoints, `set_size`).
  - A non-literal hop (`prefix: unresolved`, no `path`). A guessed URL here is S1.
  - A mount cycle.
  - Inline handlers (no `routes_to`).
  - Method constants and a method that is not a literal.
  - The family the product always reports `unresolved`.
- **Request inputs.** Every source (`query`, `body`, `path`, `header`, `cookie`, `form`, `file`, `url`, `raw`,
  `combined`), in-file and across files.
- **Entry points.**
  - Declared ones: pyproject scripts, `package.json` `bin`/`main`, Cargo `[[bin]]`.
  - In-file ones: `__main__`, Go/Rust/C `main`, shebang scripts.
  - Inherited Java `main`, selected by JLS §12.1.4.
- **Rule.** Endpoint P and R on (method, route, full path, prefix state), handler binding accuracy, input tuple
  equality, and `set_size`.
- **Ground truth.** Two authors derive it independently and reconcile, as the REST-specification studies do
  (research 30, section 5). The runtime router dump from the uncommitted copy is the oracle. A framework's own documentation is not,
  on its own.

### 7.15 Context packing

- **Cases.** A task text plus optional seeds, with expectations in the evidence-group form of localization
  benchmarks (research 30, section 5): groups are AND, alternatives within a group are OR, and "lands" means an entry carried by a slice of the packed
  plan.
- **Extreme cases.**
  - A task that names an ambiguous symbol, so the decoy definitions are `prohibited` as `required_*`.
  - A required scope across a 3-level override chain and its through-base callers.
  - A required file bigger than one slice, which must become coherent full-file slices, never a demoted or split
    required file.
  - A budget too small for the required scope, which must answer `CTX_MINIMUM_BUDGET` with exact `min_bytes`,
    `min_estimated_tokens`, `min_slices`, `min_files` and `missing`, computed by construction from the required set.
  - `max_seeds` and `max_start_nodes` overflow, which become named exclusions.
  - Generated and vendor files competing with the real one.
- **Metamorphic checks.**
  - The same request twice gives the same `canonical_hash`.
  - A run with a tiny `--timeout` resumed through cursors gives the same manifest as one uninterrupted compile.
- **Rule.** 6.5 Context metrics. Every exclusion must carry a named reason. File-level precision of the `required_*`
  entries against the case's relevant-file list is reported for information, as context benchmarks do
  (research 30, section 5), because the contract's boundary walk may require more
  than a person would list.

### 7.16 Context sessions

- **Cases.** Scripted MCP sequences with an exact expected end state.
  - Acknowledge is idempotent: credit counts once, and an unconfirmed chunk earns zero.
  - A file review is refused until the file is fully served.
  - A waiver needs a reason and never counts as coverage.
  - Advance with a stale `expected-version` is refused.
  - `complete` needs every required file covered or waived.
  - A file edited in the worktree between plan and read (the read serves the indexed bytes).
  - CRLF, empty, invalid UTF-8 and very long files read in chunks.
  - `codectx_context_include` adds seeds and bumps `scope_version`; a stale version is refused.
  - `codectx_context_entries` and `codectx_context_next` page every entry exactly once.
  - `codectx_context_record` observations land in the capsule's `accepted_facts`, `rejected_facts` and
    `contradictions` views.
  - `codectx_context_capsule` serves its six views, each paged to the end.
  - `codectx_context_close` ends the session, and later calls are refused with the documented code.
- **Rule.** Exact equality of the status fields (`state`, `ready_for_implementation`, `strict_gate_satisfied`,
  `required_files`, `fully_served_files`, `waived_files`, `guarantee_limit`) and of the error codes. The cases are
  language-independent and few (~20). The state machine itself is unit-tested already; these check the served
  contract end to end.

### 7.17 Read source

- **Cases.**
  - CRLF.
  - No final newline.
  - A BOM.
  - An empty file.
  - Invalid UTF-8 (`encoding: base64`).
  - A multibyte character straddling a chunk boundary.
  - A 3 MiB single-line file read in chunks.
  - A node read (its range bytes, no offset).
  - A path with `../`.
  - A git-ignored file.
  - A skipped symlink.
  - A post-index worktree edit, which must not show.
  - A wrong-generation node (`CTX_ARGUMENT_INVALID`).
- **Rule.** Byte equality with the committed file, or the exact error code. The oracle is the file itself.

### 7.18 Disclosure

- **Cases per project.**
  - The `calls` row's `sites`, `bound_sites`, `open_sites`, `computed_sites` and `external_sites`, which must satisfy
    `sites = bound + open + computed + external` and match the authors' count by construction (cross-checked by the
    an uncommitted syntax-tree count of call expressions).
  - `imports` (`resolved_imports`, `open_import_sites`).
  - Go `go_external_packages` and `go_external_packages_unavailable` (a project whose `replace` points outside the
    tree).
  - The per-file `structure` and dependence states for broken, oversized and non-UTF-8 files.
  - The SCIP and LSP rows for the run.
  - `providers_disabled` is empty, because every provider is on by default.
- **Answer-level.** Notices with zero precise callers, the external-supertype notice, and `requested N, effective M`
  when a request asks above a configured bound. One project carries a project config with bounds set for this
  purpose.
- **Rule.** Exact field equality, and the cross-capability `false_complete` flag (6.2).
  - **Rows not declared.** A provider row whose state is not declared in a case file must be `fresh`. Otherwise it
    is a finding: the product must work complete by default.
  - **Declared rows.** The `degraded` projects declare their expected states, and those are scored like any other
    expectation, by exact equality.

### 7.19 Incremental refresh, determinism and parity (metamorphic: no hand-written answer)

- **Refresh equals fresh.** Each language's `refresh` project has an edit script, a sequence of steps. Each step is
  one archive of whole-file replacements, plus a header listing deletes and moves. After each step:
  1. Run `codectx refresh` and collect every case answer of the project. Odd steps use the CLI command; even steps use
     the MCP `codectx_refresh_index` tool on the open session, so both refresh surfaces are covered.
  2. Index a pristine copy of the same tree fresh, at a sibling root, and collect them again.
  3. Compare the normalised answers. Any difference is a finding with its diff.
  4. Where the step also states `after` expectations (for example, a new subclass adds through-base rows), score
     them as ordinary cases.

  **Steps** (from ADR-0014's tested-edit list and the link Refresh section):
  - a body edit that changes no table;
  - a body edit adding a call;
  - a new declaration;
  - a rename, so the callers' files are republished;
  - an inserted overload, so existing overload nodes are kept;
  - a conditional redefinition;
  - a changed return value, and a changed parameter list;
  - a moved field write;
  - adding, deleting and renaming a file, including deleting the only binding of a call (the site must become open)
    and adding a file whose names now bind an open site;
  - a new subclass in a new file;
  - a header edit and a C/C++ census flip (a `.h` file changing grammar);
  - `go.mod` `require` and `replace` bumps, a module path change, a new nested `go.mod` (files move units), a vendor
    edit;
  - `package.json` `exports`, `tsconfig` `paths`, and `pnpm-workspace.yaml` globs;
  - a Python `__init__` re-export change and a Rust `pub use` change;
  - a lock file version bump;
  - a route prefix change in another file;
  - a value-flow cycle across files whose source is removed.

  **Rule.** Divergent answers must be zero: an incremental analysis "must yield the exact same results as a
  non-incremental analysis would" (research 30, section 5),
  and ADR-0014 states the same requirement.
  - **Why not just the existing tests.** `internal/index/delta` already compares stored rows. This check compares
    the served answers, which is where query-time steps (through-base climbing, candidates) could still diverge.
- **Determinism.** One project per language is indexed at two different roots. Normalised answers must be equal,
  because the order is a pure function of content.
- **Renamed copy.** Section 5.6: every case is scored again on a copy with fresh names and shifted lines, which
  catches answers that depend on names, byte offsets or line numbers rather than on the code.
- **Adapter parity.** One case each of callers, callees, refs, search, symbol and source per language through the
  CLI `--json`. Normalised data must equal the MCP answer. This is kept small on purpose: the test policy warns
  against duplicate goldens through adapters, and this is a metamorphic check with no golden.

## 8. Per-language extreme cases

Each row gives the construct, codectx's documented answer, and why. "Truth" marks where runtime behaviour differs
from the contract (scored in the truth lens, 6.3). "Rule" marks a contract question with a default, listed in
section 17 and settled before run 1. Spec anchors are named once per language; their URLs are in research 30, section 9.

### 8.1 Go

Anchors:
Selectors,
Struct types,
Method sets,
Interface types,
Implementing an interface,
Method values,
Method expressions,
Instantiations,
Type switches,
Package initialization,
Import declarations,
Build constraints,
Workspaces,
Vendoring.

| Construct | Correct answer and why |
|---|---|
| Promotion at different depths: `S` embeds `A` (`M` at depth 1) and `B`, which embeds `C` (`M` at depth 2) | `s.M()` calls `A.M`, `exact`: the selector denotes the shallowest `M` |
| Same-depth ambiguity: `S` embeds `A` and `B`, both declaring `M` | `S`'s method set has no `M`, so `implements(S, I{M})` is `prohibited`; `s.A.M()` calls `A.M`, `exact` |
| Pointer-receiver method set: only `*T` has `M` | `T` does not implement `I`, `*T` does. **Rule (A7)** |
| Interface satisfied across two modules of a `go.work`; the interface is never named by the implementing module | `implements` (`method_set`) plus `overrides` from each method |
| `io.Reader` satisfied by method set | `implements` to the external node, and `overrides` to `io.Reader.Read`'s external node |
| `var _ I = (*T)(nil)` where `I`'s package is unreadable | `implements` (`supertype_interface`), and no `overrides` |
| Overlapping embedded interfaces `I3{I1; I2}`, both with `M` | one method `M` with two declarations. Default (A18): `i3.M()` is one `calls` to each declaration, a `value_set` of 2, `virtual` |
| Struct embedding an interface field: `S struct{ io.Writer }` | `s.Write()` calls the external `io.Writer.Write`, `virtual` |
| Generics: `Map[T, U](xs, f)` with `f(x)` inside, called with `strconv.Itoa` | `Map` calls `f` by flow to `strconv.Itoa` (external). The caller calls `Map`; `Itoa` is a reference, not a call |
| Constraint method: `func Use[T Stringer](t T) { t.String() }` | calls `Stringer.String`, `virtual` |
| Method value `f := t.M; f()` and method expression `T.M(t)` | flow-bound `T.M`; the expression is `exact`, and its receiver argument flows to parameter 0 |
| `defer`, `go`, closures, a `map[string]func()` registry indexed by a computed key | calls through literals bound; the computed index is open, `value_untracked` |
| `init()` and package-level `var x = f()` | the caller is the file's module node |
| Twelve packages each declaring `Handle`; an alias import `h "x/p7"` and a dot import `. "x/p3"` | only `p7.Handle` and `p3.Handle`, `exact`; the other ten are `prohibited` |
| `vendor/example.com/lib` copy plus a nested `tools/go.mod` | imports resolve to the vendor copy; files under `tools/` are another unit |
| `foo_linux.go` and `foo_windows.go` both defining `Open` | both declarations exist. **Rule (A8):** default is a set of both |
| Type switch `case *A: v.M()` | calls `A.M`, `exact`: a concrete type's method cannot be overridden (an embedding type's own `M` is a different method, not an override) |
| Factory `func NewStore() Store` (an interface) and `NewStore().Get()` | calls `Store.Get`, `virtual`, by the declared return type; `may_dispatch` lists the implementations |
| A local variable shadowing an imported package name | the member call binds by the local's type, never to the package |
| A generated file with the `Code generated … DO NOT EDIT.` header | binds normally |

### 8.2 Python

Anchors:
The Python 2.3 Method Resolution Order,
super,
The import system,
Data model,
PEP 562,
PEP 544,
PEP 698,
functools.wraps,
importlib.import_module.

| Construct | Correct answer and why |
|---|---|
| Diamond `D(B, C)`, where `B` and `C` override `A.m` and `D` does not | `d.m()` with `d: D` calls `B.m`: C3 order `D, B, C, A, object`. `D` has no `overrides`, while `B.m` and `C.m` override `A.m` |
| Cooperative `super().m()` inside `B.m` | contract: `exact` to `A.m`, the next class along `B`'s MRO. **Truth:** for a `D` instance it runs `C.m` |
| A mixin earlier in the bases than the concrete base | `overrides` is the first definer along the MRO |
| Decorator with `functools.wraps` | the call to the decorated name binds to `f`. **Truth:** the wrapper runs first. **Rule (A9):** default is no wrapper row |
| `@property` read `obj.p` | not a call site (contract). **Truth:** the getter runs |
| `@classmethod` `cls.create()` with a subclass override | `virtual` |
| `Mod.func = repl; Mod.func()` | bound by field write (`field`) to `repl`. `setattr(obj, name, f)` is computed, so open |
| `pkg/__init__.py` re-exporting `_impl.name` through `__all__` | `imports` to the declaring file; the call binds to `_impl.name` |
| Relative imports and a namespace package (no `__init__.py`) | resolved to files |
| `importlib.import_module(f"plugins.{n}")` | open import site with fragment `plugins.`; `may_import` lists `plugins/*.py` |
| Module `__getattr__` (PEP 562) | computed |
| `getattr(obj, "name")()` and `getattr(obj, key)()` | the first as `obj.name()`; the second an open site, `computed`, no candidates (A12, 7.8.1) |
| `obj()` with `__call__` | `obj()` is call syntax, so it calls `Cls.__call__` when `obj`'s class is recovered, else it is open (`value_untracked`) |
| `a + b` with `__add__` | no `calls` (contract). **Truth:** `__add__` runs |
| A Protocol satisfied structurally | no `implements` (contract) |
| `typing.override` on a method whose base is external | `overrides` to the external base, `supertype` |
| Ten modules defining `run`; `x.run()` with `x` untyped | open, `receiver_untyped`, with ten `may_call`, same file first |
| `if sys.platform == "win32": def f()` / `else: def f()` | two declarations, disambiguated by branch |
| `class B(A): m = A.other` | Default (A19): no `overrides` (only a `def` is a method declaration); `b.m()` binds by field write to `A.other` |
| A `.pyi` stub beside its `.py` | **Rule (A10):** default is that a call binds to the `.py` implementation |

### 8.3 Java

Anchors (JLS SE 21):
§8.4.8.1 overriding,
§8.4.8.2 hiding,
§8.4.2 subsignatures,
§9.4.1 interface inheritance,
§15.12 method invocation,
§15.9.5 anonymous classes,
§15.13 method references,
§8.9 enums,
§8.10 records,
§7.5 imports,
§12.1.4 main.

| Construct | Correct answer and why |
|---|---|
| Four-level override chain across `pom.xml` modules | `callers(D.m)` gets through-base rows for every ancestor's virtual calls |
| Diamond defaults `I.m` and `J.m`, with `X.m(){ I.super.m(); }` | `I.super.m()` is `exact` to `I.m`; `X.m` overrides both |
| Static `B.s()` hiding `A.s()`; `A a = new B(); a.s()` | `exact` to `A.s`; no `overrides` |
| Package-private `m` redeclared in a subclass in another package | no `overrides` (§8.4.8.1's accessibility condition), `prohibited` |
| `private m` redeclared | no `overrides`; calls are `exact` |
| `class S implements Comparable<S> { compareTo(S) }` | overrides `Comparable.compareTo` (external, `@Override`-marked or not) by subsignature. Unmarked: the default is the contract's, ADR-0015 §2: no `overrides` row, and the callers notice says calls through the external type are found only for marked overrides |
| Anonymous `new Runnable(){ run() }`, lambdas, `this::work`, `String::length` | anonymous body is its own class (`supertype_anonymous` when external); method references bind the receiver |
| Enum constant body overriding `apply` | `overrides` `Op.apply` |
| Record with an explicit accessor implementing an interface | `implements`; the accessor `overrides` the interface method |
| Sealed hierarchy plus pattern `case Circle c -> c.area()` | `virtual` unless `Circle` is final |
| Inner class calling an outer method, and `Outer.this.m()` | bound to `Outer.m` |
| Overloads `f(int)`, `f(Integer)`, `f(Object...)` with `f(1)` | `f(int)` (phase 1 of §15.12.2) |
| `import static a.U.*` plus an explicit `import static b.U.max` | the explicit import shadows the on-demand one |
| `super.m()`, `this(...)`/`super(...)` constructor chains | `exact` |
| Interface static methods (not inherited) | callable only through the interface name |
| `Method.invoke` | external, counted. **Truth:** the reflective target runs |
| A class inheriting `main` from an abstract superclass | the inherited-main endpoint (`routes_to`, `hierarchy`) |

### 8.4 C++

Anchors:
class.virtual,
class.member.lookup,
class.mi,
class.cdtor,
expr.call,
temp.dep,
basic.lookup.argdep,
namespace.udecl,
expr.mptr.oper.

| Construct | Correct answer and why |
|---|---|
| Virtual chain with `override`, `final` and a covariant return | `overrides` direct only; a call on a `final` class is `exact` |
| `b->Base::f()` | `exact`: a qualified name suppresses virtual dispatch |
| A virtual call inside a constructor or destructor | contract: `virtual`, since it is not among the documented exact forms. **Truth:** the class's own overrider runs, recorded as a silent divergence for the owner (section 6.3) |
| A diamond with a virtual base and a unique final overrider; an ambiguous non-virtual diamond | one final overrider; the ambiguous lookup does not compile, so it is not in the set |
| `Derived::f(int)` hiding `Base::f(double)`, then `using Base::f` | without `using`: `d.f(1.0)` calls `Derived::f`; with it: `Base::f` |
| A non-virtual `Base::g` hidden by `Derived::g`; `Base* b; b->g()` | `exact` to `Base::g` |
| A dependent call `t.run()` in a template, and CRTP `static_cast<D*>(this)->impl()` | open sites (ADR-0015's template row) |
| ADL: unqualified `swap(a, b)` finds `ns::swap`, and the same call in a template on `T` | exact to `ns::swap`; in the template, an open site with candidates (A12, 7.8.1) |
| Out-of-line `void C::m()` definitions in `.cpp` with declarations in headers; reopened and inline namespaces | the declaration and definition are one method family; a call binds to the definition. **Rule (A10):** default is the definition |
| `(obj.*pmf)()`, a `std::function` member, captured lambdas | field or flow sets, else open |
| `Dog d; d.sound()`, `Dog& r; r.sound()`, `Dog make(); make().sound()` | `exact`, `virtual`, `exact` |
| `operator+` | no `calls` (contract). **Truth:** it runs |
| A header shared with C through `extern "C"`, decided C++ by census | `header_kept` and its reason exact |
| Macros generating methods; `#if` arms defining one function twice | both arms are nodes, with `set_size` on visibility macros |
| `a::util::log` against `b::util::log` with a using-directive | bound by the directive's namespace |

### 8.5 C

Anchors: ISO C draft N3220: the sections on linkage of
identifiers, function calls, generic selection and conditional inclusion. Cite the section numbers from the draft
itself when the cases are written.

| Construct | Correct answer and why |
|---|---|
| An ops table `struct ops { int (*open)(void); }` with two static instances; `ops->open()` | one `calls` per function, each with a `value_set` mark of set size 2; the sites' `dispatch` is `exact` |
| `static` functions with one name in 30 files | each file's call binds to its own (internal linkage); others `prohibited` |
| `extern` declaration in a header, definition in `a.c`, call in `b.c` | binds to the definition |
| `#define LOG log_impl` (object-like) | the macro's call calls `log_impl` (`Decl.Alias`) |
| Function-like `#define LOG(x) log_impl(__FILE__, x)` | `calls` to `LOG`, and `LOG` `calls` `log_impl` (A12, 7.8.1) |
| `_Generic((x), int: iabs, double: dabs)(x)` | exact to the selected function, else the listed set with `set_size` (A12, 7.8.1) |
| `qsort(a, n, sz, cmp)` | `cmp` is a reference, not a call from the caller |
| `#include MACRO` with the macro undefined | open import site |
| Two headers with one name under different `-I` roots | `imports` set with `set_size` |
| One function defined in both `#if` arms | two nodes disambiguated by arm |
| X-macros building an enum and a function table | the table entries are address-taken; the indirect call is a set |

### 8.6 Rust

Anchors:
method-call expressions,
traits,
implementations,
qualified paths,
use declarations,
macros by example,
conditional compilation,
renaming dependencies.

| Construct | Correct answer and why |
|---|---|
| A trait default method overridden in one impl; called through `T: Trait` and through a concrete type | `Trait::m`, `virtual`; on the concrete type, the impl's method, `exact` |
| Blanket `impl<T: Display> Describe for T`; `5.describe()` | the blanket impl's item (`Decl.Trait`), which implements `Describe::describe` |
| An inherent and a trait method of one name | the inherent method wins (candidate order) |
| `Box<Rc<T>>` auto-deref and a custom `Deref` | the method on the dereferenced type; the implicit `deref` call is truth lens only (A12, 7.8.1) |
| `<T as Trait>::m(x)` and `Trait::m(&x)` | `exact` to `Trait::m`'s impl when `T` is concrete, else `Trait::m` |
| `Vec<Box<dyn Shape>>`, `s.area()` | `Shape::area`, `virtual`, `may_dispatch` to every impl |
| Supertrait `B: A`; `a_method` called on `T: B` | `A::a_method`, `virtual` |
| `pub use crate::inner::Thing as Alias`, and a glob import shadowed by an explicit one | the explicit import wins; calls bind through the re-export |
| `macro_rules!` generating functions, and macros calling `$crate::helper()` | `generates` (`macro_transcribes` or `macro_input`) |
| `#[cfg(unix)] fn open()` and `#[cfg(windows)] fn open()` | two nodes. **Rule (A8):** default is that a call binds to the set |
| A workspace member renamed in `Cargo.toml` (`package =`) | calls resolve through the local name |
| `let f: fn() = a; f()`; `impl Fn` passed to a generic | flow or address-taken set |
| `T::default()` and `Default::default()` chosen by inference | exact to the inferred type's impl; on a generic `T: Default`, `virtual` to the trait method (A12, 7.8.1) |
| `#[path = "x.rs"] mod y` and `mod.rs` against `y.rs` | resolved files |
| Fifteen modules with `fn handle`; `use a::*; use b::handle;` | `b::handle` |

### 8.7 JavaScript

Anchors:
ResolveExport,
Exports,
super keyword,
Function.prototype.bind,
exports shortcut,
ESM and CommonJS interoperability,
conditional exports,
subpath imports.

| Construct | Correct answer and why |
|---|---|
| `exports.f = …` and then `module.exports = { g }` | the importer sees `g` only; `f` is unreachable |
| `import cjs from './c.cjs'` and a named import of a CJS export | default = `module.exports`. Default (A20): a named import binds to a statically written `exports.name =` or `module.exports = { name }`, which the runtime detects by static analysis; anything else is open |
| A barrel `index.js` with two `export *` exporting one name | the name is ambiguous and not exported (ResolveExport). The import is open, not bound to either |
| `export * as ns from`; `export { x as y }` | binds through the rename |
| `exports` conditions (`import` against `require`), `imports` `#internal` | the condition the importing syntax selects |
| `Foo.prototype.bar = function(){}`; `new Foo().bar()` | `bar` is a declaration (ADR-0017 §3); a construct-site call is `exact` |
| `class B extends A` with `super.m()` and a private `#m` | `exact` both |
| `f.call(o)`, `f.apply(o, a)`, `const g = f.bind(o); g()` | `calls` to `f`'s target, else an open site for `f` (A12, 7.8.1) |
| `` import(`./plugins/${n}.js`) `` and `require(expr)` | open import site with fragments |
| `Object.assign(Foo.prototype, Mixin)` | exact to the mixin's member when the source is known, else an open site (A12, 7.8.1) |
| `arr.map(fn)`, `emitter.on('x', h)` | references; the callers of `fn` and `h` are not the module |
| Twenty files exporting `handle`; `obj[k]()` | `computed` open site, no candidates |

### 8.8 TypeScript and TSX

Anchors:
module reference,
declaration merging,
mixins,
classes and `override`,
function overloads,
JSX.

| Construct | Correct answer and why |
|---|---|
| A class implementing an interface from another workspace package through `paths` and project references | `implements`; methods `overrides` |
| An abstract chain with `override`, and an external base with `override` | `overrides`, with `supertype` to the extended class only, never to an `implements` interface |
| An interface merged across two files; a namespace merged with a class | **Rule (A13):** default is one node per declaration site |
| Mixin `function Timestamped<T extends Ctor>(B: T) { return class extends B {} }` | `extends` to the class expression; inherited members bind through the structural type (A12, 7.8.1) |
| Overload signatures plus one implementation | **Rule (A10):** default is the implementation declaration |
| `import type { X }` | **Rule (A11):** default is that it is an `imports` row |
| An object literal satisfying an interface without `implements` | no `implements` (contract) |
| A standard decorator `@logged` on a method | the decorator's call runs at class definition. **Rule (A9):** default is no call row |
| `T extends Runner`; `t.run()` | `Runner.run`, `virtual` |
| A `.js` specifier mapping to `.ts` under `node16` and `bundler` resolution | resolved file per mode |
| TSX `<Button onClick={handle}/>` | **Rule (A9):** default is that a JSX element is no call; `handle` is a reference |
| A generic component `<List<Item>/>`, a component wrapped by a ref-forwarding function | as above |

### 8.9 Polyglot and clutter (one project)

- **One monorepo with all nine languages.** It holds:
  - a Go workspace with vendor and a nested module;
  - a `package.json` workspace mixing JS and TS (`allowJs`);
  - a Cargo workspace;
  - a multi-module `pom.xml` build;
  - Python namespace packages;
  - C and C++ sharing headers;
  - generated directories in every language;
  - the generator's hub names in every family.
- **What it scores.** Cross-family isolation: a Go `Handle` is never a candidate of a Python site, and C is joined
  with C++ only. Search filters by language and folder. Context plans on a task whose answer sits beside vendor and
  generated copies.

## 9. Size

These are estimates; the first run measures them.

| Item | Estimate |
|---|---|
| Projects | ~71. Seven per language: hierarchy, modules and clutter, dynamic and open sites, dependence, structure and search, refresh, degraded. Plus routes (4), manifests (1 polyglot), polyglot monorepo (1), read source (1), hub generator (1) |
| Cases | ~1,830: callers and callees ~315, hierarchy ~135, imports ~90, open sites ~90, references ~72, path ~27, impact ~45, dependence ~225, structure ~225, symbol ~54, search ~92, context ~30, sessions ~20, documents and classification ~20, read source ~20, manifests ~60, routes ~50, disclosure ~60, refresh ~108 steps, determinism and parity ~70 |
| Expected rows | ~10,000 written by hand, plus generator rows |
| Committed size | ~4–6 MB of sources and case files. Generator output (~15,000 files) only ever exists in scratch |

## 10. Module structure and placement

**Placement: `internal/eval/`**, a `package main` beside `internal/bench` and `internal/e2e`, the other two
verification packages in the Section 28 map.
- **Not `internal/tools/`.** Section 28 keeps that for thin CI and release automation, and this is neither thin nor
  part of CI.
- **No test files.** It is run with `go run` and has none, so `go test ./...` never touches it. CI's
  `go build ./...` and `go vet ./...` still compile it, so it cannot rot against the model types it decodes.
- **Golden data sits under `testdata/`,** which the go tool ignores, so decoys, broken files and build-tag variants
  never reach `go vet ./...`.

```text
internal/eval/
  doc.go            what it measures, how to run it, why the runner never edits expectations
  main.go           flags, startup checks, one project at a time
  anchors.go        read @tag anchors; resolve token@tag to path + byte range
  cases.go          load and validate case files (every reference resolves, every tag is used)
  materialize.go    copy a project to scratch, run its recipe, git init and commit, write the C/C++ compile database
  drive.go          codectx index/refresh/status via the CLI; one `codectx mcp serve --repo <copy>` stdio session per project; follow every cursor
  normalize.go      map nodes to references by location; drop generation-internal fields
  score.go          flags, verdicts and metrics (sections 6 and 7)
  report.go         report.json, report.md, raw/
  generate.go       seeded clutter generator
  testdata/
    projects/<language>/<project>/...     one repository root each
    cases/<language>/<project>.json       expectations, outside the trees they score
    edits/<language>/<project>/NN-<step>.archive   refresh steps: whole files plus a delete/move header
    generate/<recipe>.json                clutter recipes
docs/evaluation.md                        how to run it and read the report
docs/adr/ADR-0020-evaluation-golden-set.md   the decisions, in codectx's own terms; sources by title and URL
docs/research/30-evaluation-golden-set.md    oracle tools, versions, argv and agreement statistics
```

- **Compile databases.** C and C++ projects commit `compile_commands.in.json` with an `@ROOT@` placeholder. The runner
  writes the real `compile_commands.json` into the scratch copy, because a compilation database holds absolute paths
  and a tracked one would carry a host path.
- **Repository changes beside the new directory.** Add the line `internal/eval/` to Section 28's map of the plan. Add
  `docs/evaluation.md` to the README's documentation table.

## 11. Running it and the report

**One command.**

```sh
go run ./internal/eval --binary ./bin/codectx --out ../codectx-eval-run-1
```

- **Flags.**
  - `--binary` is the release-built binary under test, built as the README describes before the run
    (`go generate ./internal/search/textindex`, then `go build`). The runner checks it with `codectx version --json`
    and `codectx doctor --json`, and exits 3 if either fails.
  - `--out` is outside the repository.
  - `--scratch` defaults to a directory under `--out`, never `/tmp`, which is RAM-backed on some hosts.
  - `--only` takes a capability, language, project or case id, for reproducing one finding. A run with `--only` is
    never a certification run, and its report says so.
  - `--held-out` is the held-out cases' directory before they are committed. Their per-case findings go to
    `out/held-out/`, apart from the shared report, which carries their aggregates only (section 5.6).
  - `--seed` replays a renamed copy; by default each run draws a fresh one and records it.
- **Environment.** The run uses the product default: every provider on, the tool store prefetched
  (`codectx tools prefetch --all` beforehand), offline after that (`[tools] offline = true` in the run's own XDG
  config). The host toolchains `go`, `cargo` and `python3` must be present. Status rows prove what ran. A provider
  row that is not `fresh` and that no case file declares is a finding, not a skip.
- **One heavy workload at a time.** Projects are indexed strictly one after another, and queries run sequentially
  over one MCP session per project. Large trees are removed through the product's paced reclaimer
  (`internal/paced`), so a run never frees gigabytes in a burst.
- **Memory.** The runner caps its own heap (`GOMEMLIMIT=1GiB` set in `main.go`). It does not cap codectx: answers must
  not depend on memory, and capping the product is not how a user runs it. A host that wants a safety net runs the command
  inside a memory-limited control group. Tripping that limit is a finding.
- **Exit codes.** 0 when scored, whatever the scores. 2 for an integrity failure (anchors, case schema).
  3 for an environment failure (binary or toolchain missing). The scores are the result; they are never a pass/fail
  gate inside the runner.

**Report** (in `--out`, never tracked):
- **`report.json`.**
  - Run identity: binary version and commit, the repository commit and the git tree id of `internal/eval/testdata`, toolchain lock digest, host OS,
    CPU and memory, start and end times as local times with their offset.
  - Per case: flags, matched, missing and extra rows by reference, and the path of its raw answer.
  - Every aggregate table of 6.5.
- **`report.md`.**
  - Headline tables per capability × language: cases, exact %, P, R, F1, mark accuracy, S1–S5.
  - Per-tier and per-split tables, with the open-against-held-out and original-against-renamed gaps.
  - The truth-lens table.
  - Refresh divergences with diffs.
  - Every finding sorted by severity, each with its `fails` text, anchor and both answers.
  - Informational wall time and peak RSS per project.
- **`raw/<case-id>.json`.** Every page of every answer as received, so any verdict can be re-derived by hand.

## 12. Trustworthy without tests of the harness

1. **The runner never edits expectations,** and it has no regenerate mode. The set is tracked like any file.
2. **Expectations do not come from the product.** They are derived twice, the second blind, from specifications and
   the contract, and cross-checked by independent compiler-grade and runtime oracles before run 1. Each case
   records its anchor and derivation.
3. **Startup integrity checks.** These are refusals, not tests.
   - Every `token@tag` resolves to exactly one token, and every tag is used.
   - Case files validate against the schema in `doc.go`.
   - No search query collides with a tag.
   - No case file lies inside a project tree.
   - Every root occurs only in golden files (section 13, guardrail 1).
4. **Re-derivable verdicts.** Raw answers are kept, and matching is by location, the simplest rule that can be
   checked by eye. A reviewer can recompute any row of the report from `raw/` and the case file.
5. **Deterministic.** The same binary, testdata tree and seed give the same report, apart from times and RSS, and the
   determinism slice checks this for the product side.
6. **Scorer self-consistency guard.** Before scoring codectx, the runner scores each case's own `expect` rendered as
   an answer (must be `exact`) and an empty answer (must flag every expected row). This is an assertion inside the
   run, aborting with exit 2, not a test file. Nothing else depends on it, so it can be removed without
   changing any score (A14).

## 13. Keeping the product general

The golden set is small and written for the purpose. A measure that becomes a target stops measuring, suites of
small synthetic programs overstate how well an analyzer does on production code, and fixes made against one test
suite pass it while failing elsewhere (research 30, section 10). The product must stay exact and fast on any
repository, enterprise monorepos included, so these guardrails hold for every fix the evaluation prompts:

1. **No golden identity in product code.** Code outside `internal/eval/` never names a golden project, file, path,
   symbol or root, never reads `internal/eval/`, and never branches on whether it is being evaluated. The runner
   checks this at startup: it searches every tracked file outside `internal/eval/testdata/` and `docs/` for each
   root and each project directory name, and refuses with the hits (exit 2). Because roots are invented, a hit is
   never an accident.
2. **Every fix states the general rule it implements.** The commit cites the language specification section or
   the contract document, in words that hold for every program ("a private Java method overrides nothing"), never
   in terms of a case.
3. **Every fix adds a unit test on a different example.** The test lives in the product package that changed, uses
   a program written for it with different names and a different shape from the golden case, and fails when the
   fix is reverted.
4. **Exact answers.** Expectations are closed-world and carry `prohibited` rows (5.3), so a fix that returns more,
   or less certain, rows cannot pass.
5. **Renamed copies with a fresh seed every run** (5.6). A fix keyed on what it has seen fails names it has not.
6. **Held-out cases, seen once before they are fixed** (5.6).
7. **Metamorphic checks** (7.19): refresh equals fresh, two roots give one answer, and the CLI agrees with MCP.
   They are properties of every correct answer, whatever the case.
8. **The counterweight on real code.** The separate live proofs on the seven large public repositories measure
   speed, memory and completeness at scale. A fix lands only if those proofs stay within their budgets. The harness
   records time and memory for information only.
9. **No single score.** The report has no overall number: per capability × language tables with severity counts,
   so no one figure can be pushed up at the others' cost.

**How a reviewer spots a case-specific fix.**
- The startup root check fails.
- The diff holds a literal that matches the set: a name, path, file count, line number or set size.
- The change keys on a layout or a count ("a directory named …", "exactly three overrides") rather than on a
  language rule, or the cited rule does not cover the change.
- The new unit test reuses the golden program, or still passes with the fix reverted.
- In the report: a case `exact` on the original fails on the renamed copy (`name_dependent`); the open cases improve
  while the held-out cases do not; a case in another language or capability that was `exact` regresses.
- The large-repository proofs get slower or use more memory.

## 14. Repository rules

- **No third-party product names in tracked code, comments or doc bodies.**
  - The oracle drivers are never committed.
  - Expectations carry `anchor` and `derivation`, never a tool's name.
  - Research 30 is the one document that names the oracle tools, papers and benchmarks.
  - Framework identifiers appear only where code under test must import them, and in the stub APIs (A21).
  - Manifests and locks use invented package names.
- **ADR-0020** states each decision and its alternatives in codectx's own terms, for example "running other
  analyzers as live oracles". It cites sources by title and URL, as ADR-0015 does, and points to research 30 for
  the evidence.
- **No host or ignored paths.**
  - The compilation database is generated at run time.
  - `--out` and `--scratch` are flags.
  - `docs/evaluation.md` names no home directory.
- **No names of the process that produced the work** appear in the tree, the case ids or the commit messages.
- **Greenfield.** Comments say what the code does now, and there is no compatibility shim for an older case format.
- **Stands alone in a clone.** Projects build with their own toolchains and need nothing outside the tree. The runner
  needs only the codectx binary, its tool store and the three host toolchains the README already requires.
- **Original material only.** Every golden source is written for the set. Nothing is copied from another repository.
- **Test policy.** There are no new `_test.go` files.

## 15. Relation to the separate relevance benchmark

A relevance benchmark, kept outside the repository, scores search and context on large public repositories with
fixed questions.

**Recommendation: keep it separate. Do not fold it in or replace it. Run both in the final test phase and report them
side by side, never combined.**

**Why separate:**
- **Different question.** The benchmark measures realistic relevance on large real code: can search and context
  find what a person means? The harness measures contract correctness with exact answers. Neither substitutes for
  the other.
- **Licensing.** Its corpora are real repositories. One of them is licensed under terms that bar excerpts from an
  Apache-2.0 repository. Apache-2.0 and MIT excerpts would carry their headers, modification notices and upstream
  notice text. Task text taken from public datasets carries the upstream repository's licence as well as the
  dataset's (research 30, section 8).
- **It is a development set.** It has already steered ranking choices. A set that has been tuned on cannot certify.
- **Cost.** It needs several multi-gigabyte clones and one index per pinned commit.

**What the harness borrows from it:**
- the evidence-group semantics (AND groups, OR alternatives, line overlap), used by the context cases;
- blind judging with adjudication;
- the held-out idea.

**Outside this design.** Scale realism for graph answers on real code belongs to the relevance benchmark: a
graph-answer question type on its held-out repositories, which are never tuned on.

## 16. Cost

These are estimates for a 16-core, 47 GB development machine, to be measured on the first run and recorded in the
report.

| Item | Estimate | Basis |
|---|---|---|
| Index operations | ~380 per run: 71 projects, 71 renamed copies, 9 determinism re-indexes, 108 refresh steps × (refresh + fresh) | section 9 |
| Time per index | 20–120 s on these small projects. JVM startup for the Java indexer, Rust sysroot loading and the TS indexer dominate; tree-sitter and the linker take seconds | the indexers' fixed costs |
| Query time | 10–30 min for ~1,830 cases, sequential | one MCP session per project |
| Wall time | 5–9 h per full run, so 15–27 h for the three runs of section 5.6, each a separate sitting | |
| Peak RSS | about 2–3 GiB, set by the language tooling (the Rust and Java indexers) rather than codectx on small trees. The hub project (~15,000 generated files) is the largest codectx workload, about 1–2 GiB | small trees |
| Disk | under 10 GB transient in scratch, freed paced | |
| Authoring | the dominant cost: ~1,800 cases, each derived twice, plus the oracle cross-check | |

## 17. Defaults and open contract questions (each settled before run 1)

| # | Assumption or gap | Default chosen | Why |
|---|---|---|---|
| A1 | Scored surface | MCP tools; CLI for index, refresh and status and for the parity slice | agents read through MCP; the test policy discourages a second golden per adapter |
| A2 | Real repositories | not in the committed set; scale realism stays in the separate relevance benchmark | licensing (research 30, section 8), and real-code ground truth is a separate study |
| A3 | Oracle tools | used before run 1 only; named only in research 30 | the repository's naming rule; oracle drift must not move scores |
| A4 | Product configuration for the run | product defaults, every provider on, tool store prefetched, offline after prefetch | "works completely by default"; offline makes the run deterministic |
| A5 | Held-out cases | 30%, by authors outside the fix rounds, committed after run 2, then fixed | one honest look at unseen cases without leaving findings unfixed |
| A6 | Range scoring | containment scored; exact bytes informational | languages do not define where a declaration starts |
| A7 | Go pointer-receiver satisfaction | `implements` from the named type when either `T` or `*T` satisfies, with the truth note naming which | type-level hierarchy is how the contract describes Go |
| A8 | Build-tag, `#[cfg]` and `#if` variant targets | a call from shared code binds to every variant, as a set with `set_size` | the identity already keeps both; a set discloses the build-dependence |
| A9 | Decorators, properties, operators, JSX elements, TS standard decorators | not calls in the contract; truth lens records them | the contract binds call syntax only; the truth lens shows the cost (`obj()` with `__call__` is call syntax and is bound) |
| A10 | TS overloads, `.pyi` stubs, C/C++ prototypes | calls bind to the implementation or definition | that is what runs; the signature nodes stay as declarations |
| A11 | Type-only imports | `imports` row published | it is a module dependency an impact walk needs |
| A12 | Calls whose target depends on run-time values: `call`, `apply`, `bind`, `getattr`, ADL, function-like macros, `_Generic`, Rust inference and `Deref`, mixins | the per-construct answers of 7.8.1; one contract addition proposed, a `dependent` reason | what code-intelligence systems return and what the call-graph and tool-use research supports (research 30, section 11) |
| A13 | Interface merging and namespace-class merging | one node per declaration site | identity is file plus descriptor path |
| A14 | The scorer self-consistency guard | kept | it guards the scorer at no cost; it is an assertion inside the run, not a test file |
| A15 | Times in the report | local time with offset | the reader's clock |
| A17 | "Runs once" | once the code is complete: three full runs inside the final phase (section 5.6) | run 2 measures generalisation, run 3 confirms every fix; one run cannot do both |
| A18 | Go overlapping embedded interfaces `I3{I1; I2}` | `i3.M()` is a `value_set` of both declarations | the language gives one method two declarations; a set discloses it |
| A19 | Python class-level rebinding `m = A.other` | no `overrides`; the call binds by field write | only `def` declares a method |
| A20 | Named imports of CommonJS exports | bind to statically written export assignments, else open | matches the runtime's own static detection |
| A21 | Route projects | real framework imports in source, resolved to in-tree stub APIs written from scratch; the real frameworks only in the uncommitted oracle copy | detection needs the imports; stubs keep every project compiling and every precise provider `fresh` |
| A23 | LSP routing for call hierarchy | no overlay call-hierarchy cases | the model names the routing, but the MCP callers and callees input has no `semantic_source` field |
| A22 | Deliberate breakage | confined to one `degraded` project per language, with declared expected states | breakage must not cost other cases their compiler-grade facts |

## 18. Implementation order (for the plan, not done here)

1. **The runner skeleton.** Anchors, case loader, materialisation, drive and normalise, written against a
   throwaway project built from existing unit-test fixtures (section 5.4), never a golden one.
2. **The scorer and report.** Built over that throwaway project's hand-written cases, which are deleted afterwards.
3. **Projects and cases per language,** each language in two blind passes, then the oracle cross-check and
   adjudication. The held-out cases are written by authors outside the fix rounds.
4. **The generator recipes and the polyglot monorepo.**
5. **The docs and ADR.**
6. **Run 1.** Fix round (open findings). Run 2. Commit and fix the held-out cases. Run 3.
