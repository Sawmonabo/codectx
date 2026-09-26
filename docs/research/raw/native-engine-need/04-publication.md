# 04 — Publication of the native dependence families

Question 4 of the native-engine need research: which provider identity publishes the native families, how the
native producer and the hosted oracle coexist during the gates, the differential harness on the existing key
algebra, per-file incremental reindexing, the program-dependence-graph surface, and entity and alias emission.

Every `path:line` is at this branch's head. Benchmark figures are from the finished rows of the public corpora
(the benchmark run's summary lists typescript, kubernetes, home-assistant-core, elasticsearch and vscode as finished; rust is not),
fixture rows excluded, summarised per corpus and language. A figure measured on one corpus is evidence for a design,
never a constant in it.

## Evidence note

- **Read in full:** `internal/provider/dependence/graphcsv/keys.go` (339 lines), `graphcsv/emit.go` (1,301),
  `graphcsv/graphcsv.go` (446), `internal/index/delta/{delta,build,dependence}.go`, `internal/reconcile/resolver.go`,
  `internal/provider/treesitter/flow/doc.go`, ADR-0012, ADR-0010, `20-native-engine-post-mvp.md` §5 and §7.1–7.8,
  `00-mechanism-probes.md`.
- **Read in the parts the question touches:** `graphcsv/scratch.go:546-773` (entities, anchors, projection, the
  depth-8 walk), `dependence/provider.go:1-240`, `treesitter/provider.go:180-240`, `treesitter/facts.go:1-80,240-640`,
  `treesitter/worker/lower.go`, `flow/{graph,cdg,builder}.go` heads, `internal/graph/{cost.go,traverse.go:1060-1100,1320-1456}`,
  `internal/mcpserver/{registry,graph}.go`, `internal/index/{coordinator.go:340-380,780-860, generation.go:1250-1370,
  status.go:410-480}`, `internal/index/plan/{scope.go:80-130,plan.go:460-500,925-960}`, `internal/provider/registry.go:195-230`,
  `internal/storage/sqlite/schema.sql:215-232`, the nine query packs.
- **Consumers keyed on the provider id `dependence`** (grep, non-test): `index/coordinator.go:364-369` (delta applier),
  `:800` (enablement), `:844` (disabled list), `index/plan/scope.go:104` (package units), `index/plan/plan.go:479`
  (`HeavyDerived`), `app/compose.go:1087` (composition row), `graph/traverse.go:1401` (deferral fold),
  `index/status.go:768` (`units_deferred`), `config/config.go:269` (`[providers.dependence]`).
- **Consumers keyed on the relation kinds:** `graph/cost.go:15-47` (costs), `:69-78` (`DefaultRelations` includes both
  dependence kinds), `:83-89` (`ImpactRelations` excludes both), `:97-100` (`DependenceOnly`),
  `context/scope.go:34`, `context/compiler.go:1067-1070`.
- **Existing implementation considered for reuse:** the fact-key algebra (`keys.go:75-84`, `:111-222`), the
  cross-provider declaration key (`emit.go:415-417` ≡ `facts.go:402-414`), the resolver's strong-key-first lookup
  (`resolver.go:75-96`), the call-site alias join (`facts.go:601-602`, `scip/importer.go:1642`), the sealed file-unit
  reuse (`plan.go:463-472`), the flow core and the Go and JavaScript lowerings (`worker/lower.go:49-60`).
- **Not found:** no `DedupeSink` exists anywhere in `internal/` (grep for `Dedupe` finds only a test name in
  `internal/context`). The relocating algebra is `FactKey`, `KeySet`, `LoadKeySet`, `Diff`, `saveKeys`, `markDelta`.
- **Minor defect for the lead:** `keys.go:37` documents the algebra version as `"1"`; the constant at `keys.go:21` is
  `"2"`. Frozen code; recorded, not fixed.

## What the code says, before any recommendation

1. **The oracle's published graph is entity-level, not statement-level.** Entities are non-operator methods and the
   declarations `LOCAL`, `METHOD_PARAMETER_IN`, `MEMBER` (`scratch.go:611-617`). A graph node *anchors* to an entity:
   an entity to itself, a non-operator call site to its callee, an identifier to the declaration its reference edge
   names, a method reference to its method (`scratch.go:552-558`, `:619-630`). `control_depends_on` publishes
   `anchor(dependent) → anchor(controller)` for a CDG edge whose two ends both anchor (`scratch.go:676-679`); a
   controller that is an operator-rooted condition anchors to nothing and the fact is dropped
   (`providers-dependence.md` §What it publishes). `data_flows_to` walks reaching-definition edges from an anchored node
   through unanchored lowering nodes to the next anchor, at most `maxDataFlowDepth = 8` levels
   (`graphcsv.go:78-79`, `scratch.go:701-735`).
2. **The native core is statement-level.** A flow node carries a span, at most one killing definition and a kind
   (`flow/graph.go:41-47`); `Var()` returns a bare dense id with no declaration attached (`flow/builder.go:348-353`);
   `Use(n, v)` records no occurrence span (`builder.go:355-360`). The Go lowering makes one `Stmt` node per statement
   and records the whole expression's uses on it (`lower_go.go:443-450`), so a call inside a statement is not a node.
   Control dependence is emitted as `(controller, dependent)` node pairs (`flow/cdg.go:5-24`); def-use as
   `(defining node, using node)` with every φ resolved transitively (`flow/doc.go:28-33`).
3. **The core is not wired to publication.** Only Go and JavaScript have a lowering (`worker/lower.go:49-60`); the
   wire has no frame for a dependence pair, a parameter or a local (`wire/wire.go:29-44`: hello, request, source, decl,
   import, ref, done, error).
4. **Precision is per evidence row, not per provider.** `model.Evidence.Precision` is set by each emitter; the
   structural provider hard-codes `syntax` at `treesitter/facts.go:712`; the importer sets `static_analysis` at
   `emit.go:761`. Nothing in the descriptor carries precision (`model/facts.go:94-98`).
5. **A relation may be published by two units.** `relation_ids` stores one canonical row per `(from, kind, to)` and
   `relation_facts` is keyed `(unit_id, relation_id)` (`schema.sql:218-232`); `NewRelationID` has no provider component
   (`model/ids.go:85-87`). Two producers whose endpoints converge publish one edge with two evidence sets.
6. **A Required provider's unit failure fails the generation.** `generation.go:1257-1260` returns the cause for a
   Required provider instead of recording a per-unit failure; the structural provider is `Required: true`
   (`treesitter/provider.go:195-200`).
7. **Measured native volume and cost** (finished rows, per-function passes in one process under the counting
   allocator): kubernetes, Go — 228,765 functions over 17,858 files, 1,206,761 control-dependence pairs and 3,072,940
   def-use pairs before anchoring, lowering 11.6 s, the three passes 0.5 s, parse 19.2 s; the TypeScript compiler
   checkout — JavaScript 97,145 functions (92,608 / 223,736 pairs, lowering 2.1 s, passes 0.1 s), Go 25,072 functions
   (138,753 / 384,113); functions needing exit augmentation: 29 of 228,765 (kubernetes Go), 304 of 97,145 (TypeScript
   checkout JavaScript). Lowering, not analysis, is the native cost; both are well under parse on the Go corpus.

## (a) Which provider identity publishes the native families

**Recommendation.** The four file-local families — `control_depends_on`, `data_flows_to`, `reads`, `writes` — are
published by the **structural provider** (`treesitter`) as four capabilities beside `structure`, per file unit, at
`static_analysis` precision on their evidence. `calls` resolution, when ported, is published by a **new
package-scoped linking provider** that depends on `treesitter` and publishes *aliases* at call-site keys — the join
the precise tier already uses (`facts.go:601-602`, `scip/importer.go:1636-1645`) — plus `calls` edges only where the
structural tier has no site to alias. The `dependence` id is never reused. Required changes this implies:
- the structural evidence helper takes precision per relation kind (`facts.go:712` today fixes `syntax`);
- the descriptor's capability list grows (`treesitter/provider.go:195-200`); `lang.Version()` (`lang/lang.go:138-140`)
  folds the lowering and flow versions, so a lowering change invalidates structural units;
- a native-analysis failure inside a file never fails the unit: because the provider is Required (fact 6), a crash or
  defect in the pass degrades only that file's four capability rows (`failed`/`partial` with a reason) while
  `structure` stays `fresh`. On a worker crash during analysis, the retry re-parses with analysis off for that file;
- the deferral fold (`traverse.go:1361-1404`) and `DependenceOnly()` (`cost.go:97-100`) are consumers of the engine's
  deferral only; while a language is engine-published they stay correct (they over-disclose for a graduated
  language's node, never under-disclose), and they are deleted when the last language graduates (phase 3).

**Strongest alternative, steel-manned.** Keep the product identity `dependence` and give it a native backend. Every
one of the nine id-keyed consumers above stays untouched; the capability rows an agent reads keep their provider
name; the ADR's claim "a consumer sees no change of label" (ADR-0012:337-338) is then true of the provider as well
as the precision; and a per-language backend switch inside one provider is a small, local change.

**Why the recommendation wins.** (1) The phase-3 gate is "a store query over a fresh index returns zero
dependence-provider rows of the four dependence kinds" (ADR-0012:278) and retirement condition 2 is "a fresh index
with the dependence provider absent publishes every capability at the same state" (ADR-0012:288-289): both are
unsatisfiable as written if the native rows carry the id `dependence`. (2) A descriptor has one invalidation scope:
`dependence` is `InvalidationPackage` (`dependence/provider.go:209-214`) and the planner builds its units from
package inventory with `heavy: true` (`plan/scope.go:104-128`, `plan.go:479`); the four families are file-local by
measurement (ADR-0012:116-120), so publishing them there forfeits per-file reuse. (3) The analysis must run in the
worker that holds the tree (ADR-0012:157-160); a `dependence` unit runs after `treesitter` (`dependence/provider.go:47`) in the
coordinator, so it would re-parse every file. (4) One provider with two backends selected per language is the dual
producer a greenfield product forbids; two provider ids with a per-language publication set that shrinks to empty is
a terminating gate. A new id for the four families is rejected for the same reason as (3): a second provider cannot
share the structural worker's parse pass without a second parse. The linking step needs its own id because it is
the one package-scoped family (ADR-0012:166-167) and the structural descriptor is `InvalidationFile`.

**Trade-off accepted.** Time to the base generation grows by the native pass, because the structural provider is
Required and blocks it, where the engine families today arrive later as background work under `auto`
(`providers-dependence.md` §enabled). On kubernetes Go the in-process lowering plus passes is 12.1 s against 19.2 s of
parse (fact 7). Agents also read the four capabilities under `treesitter` instead of `dependence` — a visible rename
of the capability's provider, not of its precision.

**The measurement the benchmark task must take.** On every corpus class (one large repository per language family
and the mixed monorepos), indexed end to end on the same host: time to the base generation and time to every
capability `fresh`, with the native pass and with the engine path, and the share of structural-unit CPU spent in
lowering and analysis. Pass: time to every capability `fresh` is lower natively on every class; the base-generation
increase is reported per class with its share (it is a disclosed cost, not a gated one).

## (b) Coexistence with the hosted oracle during the gates

**Recommendation.** Both run; one publishes, decided **per language at compile time** by one shared predicate — the
set of languages whose native families have passed their gate. The structural emitter publishes the four families
only for files of a language in the set; the importer's projection drops the same four kinds for files of a language
in the set (a filter on the file's language in the control-dependence insert at `scratch.go:676-679`, the data-flow
insert at `:729-735` and the reads/writes derivation `deriveReadsWrites`, called at `graphcsv.go:353` — the minimal
change that keeps the oracle's export correct). The shadow is **not in the product**: for a language not yet
in the set, the differential harness (c) runs the native lowering in process over the pinned corpus snapshot — the
same in-process path `internal/bench/flowbench_test.go` already drives — and writes a comparison key file; the engine
side is read from the published rows of an ordinary product index of that corpus. Nothing is attached to a
generation, no product store holds shadow rows, and no user run pays for verification. A language joins the set in
the commit that records its gate; the engine's comparison key files from that gate are kept with the corpus's
benchmark artifacts and are the oracle for every later regression and for retirement condition 1. It ends when the
set holds all nine languages: the importer's four-kind projection is deleted (phase 3), then the provider, its
backend, the import package less the relocated comparator, and every id-keyed consumer listed in the evidence note
(phase 6, which is retirement condition 5).

**Strongest alternative, steel-manned.** A shadow unit in the product: the native pass runs for every file of every
lowered language, streams its keys to a delta-state artifact the unit stores but the generation never reads, and the
product diffs it against the engine unit's stored key set on every index. The gate is then measured on real user
repositories continuously, not only on pinned corpora, and a regression shows up the day it happens. Storage even
permits both to publish (fact 5), so a softer variant lets both publish and relies on endpoint convergence.

**Why the recommendation wins.** The stored engine key set cannot be diffed against a native one by digest: its
owner component is the engine's full name and its endpoints are identities the engine minted for locals and
parameters (c, below), so an in-product diff needs the same normalising harness anyway, plus a stored artifact and a
second code path in every user's index — speculative infrastructure the policy forbids. Letting both publish is not
safe where endpoints do not converge: a call-anchored endpoint (c) yields two different relations for one fact.
Gates are stated on pinned corpora per repository class (ADR-0012:273-281), so continuous product-side diffing
measures nothing a gate reads.

**Trade-off accepted.** The frozen importer receives one filter per phase flip, and after a language graduates its
engine rows exist only as recorded comparison files, so a later engine-side regression cannot be observed without a
dedicated oracle run. Accepted: the engine version is pinned and is being deleted.

**The measurement the benchmark task must take.** Per language flip, on that language's corpus: a fresh index's
store query for relations of the four kinds grouped by publishing provider and file language. Pass: rows for the
flipped language come from `treesitter` only, rows for every other language from `dependence` only, and the relation
count for the flipped language is within the gate's band of the pre-flip count.

## (c) The differential harness on the existing key algebra

**Recommendation.** Relocate `FactKey`, `KeySet`, `LoadKeySet` and `Diff` (`keys.go:75-222`) into the harness, and
feed both producers through one comparison key computed from what each side has — the engine's published rows, the
native pass's in-process output — from components both can compute the same way by construction:

| component | engine side | native side | by construction equal? |
|---|---|---|---|
| label | `rel:<kind>` (`emit.go:842`) | same | yes |
| owner | engine full name (`keys.go:42-43`) | — | **no**: replaced on both sides by the declaration key of the innermost function whose range contains the site |
| file | root-relative path | same | yes |
| operator | engine operator name for reads/writes (`keys.go:46-47`) | — | **no**: blanked on both sides |
| target | syntactic name | same | yes |
| positional | verified evidence range, whole-line fallback when the text does not match (`emit.go:291-356`), refused at 1,000 characters (`graphcsv.go:90-95`) | exact occurrence span | only where the engine matched its text; a pair differing in positional alone is classed `positional` |
| endpoints | published node identities (`emit.go:843`) | — (the native side is never published during a gate) | **no**: replaced on both sides by each endpoint's declaration key `decl:<name>@<path>:<l1>-<l2>` (`emit.go:415-417`), computed from the stored node's file, name and range on the engine side and from the lowering's declaration span on the native side; a callee with no location is `<resolution class>:<name>` on both sides; callee anchors still differ until `calls` is ported |

Each side's key line is the 64-character digest followed by its pre-image; `Diff` compares the digest prefix and
hands the whole line to the classifier, so one sorted format and one merge serve both the product's delta path
(digest only) and attribution. Engine keys are sorted by the store read; native keys per file are sorted in memory
(one file's facts) and k-way merged, because `LoadKeySet` refuses an unsorted file (`keys.go:145-147`). The
comparison is restricted to the engine unit's own file set — whole packages for Go (ADR-0012:276).

The signed per-cause band for `data_flows_to` (ADR-0012:325-327; mechanism in
`18-algorithms-dominance-and-dataflow.md` §What the choice costs): each unmatched key carries a sign (+ native-only,
− engine-only) and a cause decided mechanically, not by hand:
- **φ-depth consumption** — expected zero, because the native def-use resolves φ transitively (`flow/doc.go:28-33`);
  any residue is a defect;
- **engine over-kill** (name and code equality kills) — expected +;
- **engine substring over-connection** (`sameVariable` text containment) — expected −;
- **engine depth-8 truncation** — expected +: the native walk has no depth, the engine's stops at 8
  (`scratch.go:714-726`);
- **call-anchored endpoint** — ± pair: the engine anchors a call to its linker's callee, the native pass to the
  structural callee (f);
- **synthetic initialiser** — engine-only facts owned by a `<clinit>` owner, keyed by content digest
  (`emit.go:699-704`) — expected −;
- **positional only** — ± pair with equal endpoints;
- **cross-file global** — expected − for Go package variables declared in another file of the package.

For `control_depends_on` the same machinery, with causes **exit augmentation** (+; the function's row has
`augmented > 0`), **missing post-immediate-dominator truncation** (+), **operator-rooted controller** (the engine
drops it; + if the native pass anchors a condition to its operands), **try modelling** (± pair: the engine names the
last statement of a `try` body as controller, `providers-dependence.md` §What it publishes), **Rust try operator** (+),
call-anchored endpoint and positional only. `reads`/`writes`: resolution causes (the four measured gaps,
`20-native-engine-post-mvp.md` §5) and unresolved → `may_refer_to`.

**Strongest alternative, steel-manned.** Diff the engine's stored per-unit key set (`delta/dependence.go:22`,
written by `saveKeys`, `keys.go:270-313`) against native keys built by the same `FactKey` call, digest for digest: no
new key definition, the exact algebra the delta path already trusts, and a zero-cost engine side because every
product index already stores it.

**Why the recommendation wins.** A digest is one-way, so an unmatched key cannot be classified by cause — and the
signed per-cause band is the gate. And two of the seven pre-image components (owner, operator) are engine vocabulary
a native producer cannot emit without imitating the engine's naming, which would re-import the engine into the
native producer; endpoints for locals and parameters are engine-minted until (f) lands.

**Trade-off accepted.** The comparison key discards owner and operator, so a fact attributed to the wrong owning
function with the same site and endpoints is not caught by the key; the owner is instead implied by the site's range.

**The measurement the benchmark task must take.** Per language and family, on its pinned corpus: the band from two
engine runs first, then one native run; report signed counts per cause and the unclassified residue. Pass: the
unclassified residue's symmetric difference is within the engine-versus-engine band for that family, the φ-depth
cause is zero, and every named cause has the sign it is predicted to have; a cause of the wrong sign fails the gate.

## (d) Per-file incremental reindexing

**Recommendation.** The four file-local families are part of the structural file unit, so they inherit its reuse
unchanged: a unit is reused only on a full fingerprint match of the file's content and the provider version
(`plan.go:463-472`, ADR-0012:178), and an edited file is re-lowered and re-analysed alone. No delta applier, no key
set, no carry-over: the dependence applier's machinery exists because the engine renumbers ids and has no per-file
mode (`delta/dependence.go:24-33`). For `calls`, the link step is one unit per package scope of the linking provider,
whose inputs are two per-file tables the structural unit already almost publishes: the file's **exported names**
(it publishes `exports` relations, `facts.go:319-321`, and package-scope aliases for Go and Java, `facts.go:343-349`)
and its **unresolved call sites** (the `call:` placeholder callees, `facts.go:548-575`). The link unit's fingerprint
folds the digest of every member file's exported-name table and each file's site table, not the file's content. That
is a model addition, not a reuse: unit inputs today are files, yielded in `FileID` order and folded by
`model.NewUnitInputHasher` against `Build.Spec.InputHash` (`delta/delta.go:56-72`); whether `UnitSpec` can admit a
derived artifact as an input was not read and is unavailable here. Under that addition a
body-only edit that changes no export and no unresolved site does not invalidate it. When an export digest changes,
the link re-resolves only the sites whose callee name is in the changed name set (a ledger name → sites, streamed
from the sealed site tables, never held whole); a change to a declared subtype relation re-resolves the sites whose
receiver type is in that relation's closure.

**Strongest alternative, steel-manned.** Rebuild a package's link step whenever any member file's structural unit
changes. It is one invalidation rule, it cannot miss a dependency the name ledger fails to model (a new overload, a
changed signature, a star import), and a link over tables is a merge join that is cheap next to parsing.

**Why the recommendation wins.** Package size is a property of the repository, and one of the reference units is
4,984 files (ADR-0012:374-375); rebuilding it on every keystroke-sized edit is the "one edited file re-runs a whole
unit" cost the ADR records against the engine (ADR-0012:487-489). The digest rule falls back to the full rebuild
exactly when the export surface changes, so it is never less complete — only faster when bodies change, which is
the common edit.

**Trade-off accepted.** The export and site tables become part of the structural unit's contract, and a name-ledger
defect is a stale `calls` edge rather than a slow one; the full-rebuild path is the recovery.

**The measurement the benchmark task must take.** On each corpus class: a scripted body-only edit and an
export-changing edit to one file of the largest package, reindexed. Pass: the body-only edit rebuilds one structural
unit and no link unit; the export edit rebuilds one structural unit and one link unit whose re-resolved site count is
reported; in both cases the resulting store's `calls` key set equals a fresh full index's.

## (e) The program-dependence-graph query surface

**Recommendation.** No new tool. Both kinds are already in `DefaultRelations` (`cost.go:69-78`) and share the
`(from, kind, to)` identity with every other relation (`schema.sql:218-225`), and the per-generation adjacency stores
each node's neighbours as sorted deltas with a one-byte kind code (ADR-0005:63-65), so an entity pair's control and
data edges are adjacent in one node's list: composing them is a streaming group-by over that list, O(degree), no new
index. Add: (1) `ProgramDependence()` beside `DefaultRelations`/`ImpactRelations` in `cost.go`, the two kinds;
(2) `codectx_impact` expands over it — `ImpactRelations` excludes both kinds today (`cost.go:83-89`), so impact never
walks the PDG; (3) graph and path answers group a pair's relations into one composed item carrying both kinds when
both join, with each relation's evidence site ranges paged under the answer's existing cursor, because a graph answer
returns no evidence today (`providers-dependence.md` §What it publishes) and a PDG edge without its site does not
say which statement depends on which. Pagination and work budgets are the existing ones (ADR-0001 §2.2;
ADR-0012:260-263), defaulting to unlimited. `codectx_callers`/`codectx_callees` already walk both kinds by default
(`traverse.go:1077-1079`).

**Strongest alternative, steel-manned.** A dedicated `codectx_program_dependence` tool (forward and backward slice
from a node, statement-granular): the name tells an agent the capability exists, the answer can be shaped for slicing,
and callers/callees stay pure call neighbourhoods as their descriptions promise (`registry.go:48-51`).

**Why the recommendation wins.** The tool surface is fixed at 23 by the specification (`registry.go:92-94`), and ADR
§3's obligation is that the *product* composes the graph so the caller filters nothing (ADR-0012:193-197), which the
existing traversal plus the composed item satisfies. A slicing tool would be a second traversal entry point over the
same adjacency.

**Trade-off accepted.** The answer is entity-granular, with statements carried only as evidence ranges; a
statement-level PDG would need statement entities, which the store does not model.

**The measurement the benchmark task must take.** On each corpus class, for a seeded sample of functions: the
callees answer with default relations, and impact before and after `ProgramDependence()` joins `ImpactRelations`.
Pass: every entity pair holding both kinds appears as one composed item carrying both, each with at least one evidence
range; page latency against the adjacency is reported per class (not gated).

## (f) Entity and alias emission

**Recommendation.** The native pass emits, from the structural builder in the parent (the worker sends spans and
names, the parent resolves identities, as it does for declarations):
- **functions and methods** — none new: edges attach to the builder's own declaration identities (`facts.go:280-330`),
  which already carry the cross-provider declaration key alias (`facts.go:333-335`); the engine's method resolves to
  the same node through its strong key (`emit.go:578-586`, `resolver.go:75-96`).
- **anonymous callables** (function literals, lambdas, JavaScript class bodies, which the lowering treats as
  functions, `worker/lower.go:22-27`) — a `function` node, located, qualified by the enclosing declaration; it has no
  identifier token, so it cannot merge with the engine's synthetic method name — a named harness cause.
- **parameters and locals** — `variable` nodes (the engine's kind for `LOCAL` and `METHOD_PARAMETER_IN`,
  `emit.go:514-523`), located at the declaring identifier, qualified `<enclosing qualified name>.<name>`, scope
  `file:<path>`, and the alias `decl:<name>@<path>:<line>-<line>` — byte-for-byte the key the importer mints for the
  engine's declaration (`emit.go:415-417`, `:578-586`), so during the gates the engine's local and parameter endpoints
  resolve to the native identities once a language publishes (the harness itself needs no store identity: its endpoint
  component is the declaration key, (c)). Today the
  structural provider publishes no parameter (zero `param` captures across the nine query packs) and locals only
  where a pack's variable pattern matches (`go.scm:16` `var_spec`, `ecmascript.scm:18` `variable_declarator`, both
  unanchored to module level; Python, C and C++ patterns are module-level); a local the builder already declares is
  reused, never minted twice. Two declarations with one name on one line share that key: the alias is omitted and
  counted, as `declKey` omits an over-long key (`facts.go:390-400`).
- **fields** — the file's `field` declarations by name where the receiver's type is declared in the file; otherwise
  the target is published as `may_refer_to`, as the importer does for an unresolved target (`emit.go:587-593`).
- **cross-file names** (a Go package variable in another file) — a provider-local reference node keyed like the
  `call:` placeholder (`facts.go:548-575`), never a guess at another file's node.
- **call anchors** — the structural callee of the same call site (the in-file declaration or the `call:` node), which
  the precise tier and later the linking provider join through the call-site alias (`facts.go:601-602`).

The native **anchors table** is per function and replaces the engine's (`scratch.go:619-630`): a node's anchors are
the declaration of the variable it defines, the declarations of the variables it uses, and the callees of the calls
it contains, each with its occurrence span. `control_depends_on` is published from every anchor of the dependent to
every anchor of the controller; `data_flows_to` from the declaration a def-use pair's definition defines to each
anchor of its use, one hop with φ resolved — no depth bound, because the engine's eight levels walk expression
lowering nodes the statement-level core does not have. This needs three additions to the core the seed does not
have: a declaration span on `Var()` (`builder.go:348-353`), an occurrence span on `Use`, and the call spans a node
contains (`graph.go:41-47`), plus new bounded wire frames for local declarations and dependence pairs
(`wire.go:29-44`).

**Strongest alternative, steel-manned.** Mimic the engine's projection exactly: anchor a controller only when the
condition's root is an identifier or a non-operator call, and publish only facts the engine would. The gate's
unclassified residue shrinks, parity is easier to prove, and consumers see no change in which facts exist.

**Why the recommendation wins.** The engine's anchoring drops every control dependence whose condition is an
operator expression (`x < y`), which is most conditions, and `control_depends_on` is a single-hop join, so a dropped
edge is an unrecoverable fact (ADR-0012:175). Reproducing a loss to pass a diff is the wrong gate; the harness's
signed causes (c) exist precisely so a surplus with a named cause is admitted.

**Trade-off accepted.** The native graph publishes more `control_depends_on` and more `data_flows_to` than the
engine, so the gate reads causes, not a symmetric difference; and store rows grow with parameters and locals as
nodes, which the engine already publishes today under its own id.

**The measurement the benchmark task must take.** On each corpus class: the share of engine local and parameter
endpoints that resolve to a native identity through the declaration key (the engine's resolution basis
`native_key` on those nodes), and the native node and relation counts per source byte against the engine's. Pass:
every engine local or parameter that has a native declaration on the same line resolves to it, the residue is only
same-line same-name collisions and multi-line declarations, and the count ratios are reported per class.

## Recommended publication design

1. The structural provider publishes `control_depends_on`, `data_flows_to`, `reads` and `writes` per file at
   `static_analysis`, from the worker walk that parses the file; a pass failure degrades those four rows for that
   file, never the unit. A per-language publication set, one predicate shared with the importer, decides which
   producer publishes; it grows one language per gate and is deleted at phase 3.
2. The shadow is offline: the harness runs the native lowering in process over pinned corpora and diffs a
   comparison key in which owner and operator are normalised away and both endpoints are the existing cross-provider
   declaration key; unmatched keys are classified into signed causes and the unclassified residue is
   gated against the measured engine band.
3. Parameters and locals become structural `variable` nodes carrying the cross-provider declaration key, so both
   producers' endpoints are one identity during the gates and the engine's `LOCAL`/`PARAM` resolve to them.
4. `calls` moves last to a new package-scoped linking provider that publishes call-site aliases, invalidated by
   exported-name and site-table digests, not by file content.
5. The PDG is composed in the existing traversal and impact answers over `ProgramDependence()`, with evidence ranges
   paged per pair; no tool is added.
6. The engine ends when the set holds all nine languages and the linker covers `calls`: the import projection, the
   provider and its backend, and the nine id-keyed consumers listed above are deleted in one change.

**Unavailable.** Native relation counts after anchoring (nothing publishes them yet); store growth from parameters
and locals as structural nodes (the engine's local and parameter node count per corpus was not read); per-language
native cost for Python, Java, C/C++, Rust and TypeScript/TSX (no lowering exists); the base-generation wall on a real
index with the native pass (no wired producer); Rust bench rows (the corpus run had not finished).
