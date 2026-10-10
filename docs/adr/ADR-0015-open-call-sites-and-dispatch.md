# ADR-0015: Unbound calls are open sites with name candidates, and calls through a base class are found at query time

## Status

Accepted, 2026-10-09, by the owner's rulings on three questions: name-only candidates on by default as a new
relation; calls through a base class stored against the declared method with the dispatch view computed and marked;
the fixed in-file candidate cap removed. Not yet implemented. The survey and hands-on tests behind it are
[research 24](../research/24-call-resolution-open-sites-and-dispatch.md).

## Context

- **Silent omissions.** A call site the linker cannot bind publishes nothing, so "who calls `handle`?" can return a
  bare zero while a call names it.
- **A silent undercount.** A site bound to two or more justified targets (`a.sound()` with `a: Animal`, `Dog` and
  `Cat` overriding `sound`) publishes `calls` to a placeholder node and `may_refer_to` to each target. Default walks
  exclude `may_refer_to`, so "who calls `Dog.sound`?" misses `speak`.
- **No hierarchy facts.** No native provider publishes `extends`, `implements` or `overrides`, although impact
  analysis and context planning list them first. The linker computes the hierarchy, Go's method-set satisfaction
  included, and keeps it internal.
- **A fixed count.** The structural provider keeps at most 16 in-file ambiguous candidates (`MaxAmbiguousCandidates`)
  and counts the rest as dropped.
- **The requirement.** Complete answers by default, nothing to tune, never silent about what could not be done, no
  fixed counts on generated data or exact lookups.

## Decision

### 1. Every call site is classified by what justifies it

| Basis | Example | Published |
|---|---|---|
| One justified target | a direct, imported or scoped call; a call whose receiver has a declared type | `calls` to the target (for a declared type: the method that type declares or inherits) |
| Several classes by value flow, no declared type | `def speak_u(a): a.sound()` with `Dog` and `Cat` objects flowing in | one `calls` per class's method, its evidence carrying the set size |
| Several functions by signature or by field | `fp()` through a function pointer with two address-taken functions of its signature; `obj.run()` where two functions are written to a field `run` | one `calls` per function, its evidence carrying the set size, `dispatch: exact` |
| Name only | `obj.handle()` with no recovered type; a call in a C++ template on a type that depends on a template parameter | an open-site node and `calls` to it; candidates at query time (decision 3) |
| No name | `obj[k]()` | an open-site node, reason `computed`, counted, no candidates |
| Proven external | a library call, a builtin receiver | the existing external placeholder, counted as external |

A name never binds `calls`, including a name with exactly one declaration in the repository: a library method of
the same name is invisible to the index.

### 2. Calls through a base class are stored once and found at query time

- **Stored.** `speak calls Animal.sound`, even when `Animal.sound` is abstract or an interface or trait method; no
  placeholder node, no `may_refer_to`, no edge per override. Each `calls` occurrence's evidence carries a
  `dispatch` field of its own: `virtual` when the call can run an override, `exact` when it cannot (`super`, a
  qualified call, a constructed object, a final, static or non-virtual method). The field is computed per site, not
  read back from the technique that bound it.
- **Hierarchy.** The linker publishes `extends` (type to declared supertype), `implements` (type to the interface or
  trait it declares or, for Go, satisfies by method set) and `overrides` (method to the method it directly overrides
  or implements). A supertype outside the indexed project gets a structural node keyed by its qualified name, so the
  `overrides` edge has an endpoint and the walk does not stop at the project boundary.
- **A relation and its occurrences.** One `calls` relation can hold several occurrences, a `super` call and a
  virtual call into the same method. The relation counts as through-base when any occurrence is `virtual`, and a
  through-base row lists only those occurrences.
- **A method of an external supertype.** It gets a structural node keyed by the supertype's qualified name, the
  method's name and its disambiguator (ADR-0014). It is the same node a call on a receiver of that external type
  already targets, so the walk joins the two. A method overrides an external one when the language marks it: Java
  `@Override`, C++ `override` or `final`, TypeScript `override`, a Rust trait `impl` item, Python `typing.override`,
  or a Go method that satisfies an external interface's method set. An unmarked method of a class with an external
  supertype publishes no `overrides` edge, and its callers answer says that calls through the external supertype are
  found only for marked overrides.
- **Callers of `m`.** Calls into `m`; then, for every method `b` that `m` overrides at any depth, the calls into `b`
  whose `dispatch` is `virtual`, each row marked "through `b`" with `b`'s override count. The answer counts `direct`
  and `through_base` apart and includes the `overrides` relations it used, so the route
  `speak -calls-> Animal.sound <-overrides- Dog.sound` is visible. The step belongs to the `callers` operation, not
  to the relation list, so a request for `calls` alone still gets the marked rows. An override's direct callers are
  never added to a base method's callers: `direct(d: Dog)` does not call `Animal.sound`.
- **Callees, paths and forward impact.** `callees(speak)` lists `Animal.sound`, then its overrides at any depth
  marked "may dispatch to", counted apart. Paths and forward impact take the same downward step at the cost of an
  `overrides` hop.
- **Impact of `m`.** Its callers as above, plus the methods that override it when `m` is a base, since a signature
  change breaks them.
- **References of `m`.** The whole method family, both ways: references serve rename and reading, not call graphs.
- **Precision.** For a declared type the set is every override (class-hierarchy analysis): a library's index cannot
  see which subclasses other code will construct, so narrowing by the classes constructed in the project would drop
  real callers. Where value flow already gives the exact classes (decision 1, second row), the published targets are
  those classes only. Every through-base and may-dispatch row is marked as possible, never as certain.
- **Cost.** One index lookup per hierarchy level per visited node; exact totals, paged, no cap.

**Alternatives.** A separate relation kind for dispatch calls: a request filtered to `calls` drops it, the same
undercount; no surveyed tool does it. A `calls` edge to every override: one edge per site and override (a base with
1,000 overrides called from 5,000 places is 5,000,000 edges), and a new subclass changes the facts of every calling
file; with an abstract base, the base itself has no callers. Treating the method family as one symbol for calls: it
lists `direct(d: Dog)` under `Cat.sound`, which never runs. The through-base step only on request: the default answer
for `Cat.sound` is then a false zero.

### 3. An unbound site is one open-site fact, and its candidates are computed per query

- **Published once per site.** An open-site node in the file's scope, located at the site, named by the callee, with
  metadata `resolution: unresolved`, a `reason` (`receiver_untyped`, `value_untracked`, `global_unbound`,
  `computed`), the call `shape` (`bare`, `member`, `value`, `construct`) and the argument count. The caller has a
  `calls` edge to it. In answers this node replaces the structural `call:<name>` placeholder for the same site.
- **Candidates per query, never stored.** The declarations of the same generation with the same name (as the
  language compares names), the site's shape (methods and function-valued fields for a member site, functions for a
  bare or value site, classes for a construction), the same language family, and, in the static families only, a
  fitting argument count. Storing them would cost one edge per site and same-name definition and would change one
  file's facts when another file adds a declaration.
- **Relation kind `may_call`,** from the caller to each candidate, precision `heuristic`, resolution `name_only`, or
  `unique_name` when exactly one compatible declaration exists. Not `may_refer_to`, which is identity
  reconciliation.
- **Order.** Every precise result first; then candidates by closeness: same file, same directory or package, a module
  the site's file imports (or that imports the candidate's file), same unit, then elsewhere, other units included;
  within a step, names with fewer compatible definitions first, then a stable key.
- **Per operation.** `callers` lists incoming `may_call` one hop out and walks through them only when the request
  names `may_call`; `callees` adds the open-site node and its candidates; `references` adds `may_call` sites as
  heuristic occurrences after the precise ones; `path` admits `may_call` at the highest cost only when no precise path
  exists, and says so; `impact` counts the open sites naming a frontier node at each level as possibly affected and
  does not expand them unless asked; context plans admit candidates as optional context below every precise
  relation's weight and say how many were left out of required scope.
- **Never a bare zero.** When precise callers are zero and candidates exist, the answer says so; when none exist, it
  states how many of the project's sites are bound and open.

### 4. Disclosure is structured and exact

- The linker's `calls` capability row carries exact `sites`, `bound_sites`, `open_sites`, `computed_sites` and
  `external_sites` counts.
- Every call answer carries structured counts (`direct`, `through_base`, `may_dispatch`, `may_call`) and a notice in
  plain words, for example: "3 call sites in 2 callers name `handle` and no analysis bound them. They are listed by
  name only and may not call this symbol."
- A name with 7,188 definitions reports `candidates: 7188` and a ranked first page; no bail-out above a count. A
  deadline that stops a count reports "at least n" under the existing truncation contract.
- Tool descriptions say in one sentence that `may_call` is name only and that through-base rows may not run.

### 5. No fixed candidate count in the structural provider

`MaxAmbiguousCandidates` is removed: every in-file candidate justified by scope is kept, uncapped. An in-file member
call on an untyped receiver is name only, so it becomes an open site with `may_call` candidates (decision 3), not an
in-file `may_refer_to`.

### 6. Dynamic includes follow the same rule

An import whose path does not fold to a constant (`include "{$x}.php"`, `require(expr)`, `import(expr)`,
`importlib.import_module(expr)`, `#include MACRO` with the macro undefined in the file) is an open import site with
its literal fragments; candidates are the files whose path matches them, published per query as `may_import`,
ranked, paged and counted.

### 7. Proof

- The six-server fixture of research 24 (two subclasses, three levels with a `super` call and a qualified call, an
  interface case) gives, for every language: `callers(Dog.sound)` = `direct` plus `speak` marked through
  `Animal.sound`; `callers(Animal.sound)` = `speak` only; `callers(Puppy.sound)` includes `speak` and `walk`; the
  `super` and qualified calls never appear as through-base rows; `callees(speak)` lists `Animal.sound` and its
  overrides marked.
- A function called only through `obj.handle()` with unknown `obj` returns its possible callers, never "none"; a
  name with thousands of definitions returns an exact total and a ranked first page.
- `may_call` precision and recall are measured per language against compiler-grade ground truth on the fixtures and
  published with the linker's documentation.

## Consequences

- Answers stop being silently empty or short; every possible row says it is possible.
- Storage grows by one fact per open site and one `overrides`, `extends` or `implements` edge per hierarchy link, not
  by the name cross-product or the override fan-out.
- A new subclass changes only its own file's facts.
- Graph answers need per-row marks and counts, which the structured answer change provides.

## Sources

- [research 24](../research/24-call-resolution-open-sites-and-dispatch.md) and [raw/call-resolution](../research/raw/call-resolution/)
  — the survey, the six language servers' replies, the scanner and analyzer runs, and their full source lists.
- *Navigating the call graph*, https://codeql.github.com/docs/codeql-language-guides/navigating-the-call-graph — a
  static callee per call beside a "may call at runtime" view.
- *Call graph*, https://kythe.io/docs/schema/callgraph.html — calls to the most specific method plus override edges,
  callers found by walking the override chain.
- *scip.proto*, https://github.com/sourcegraph/scip/blob/main/scip.proto — references of an override include
  references of the method it overrides.
- *Show callers of base functions in incomingCalls*, https://llvm.googlesource.com/llvm-project/+/378e4ed1acf94ee911e2ccfda0045ac3fdc9e291
  — callers through a base flagged "may or may not actually call".
- *jdt-ui-dev thread*, https://www.eclipse.org/lists/jdt-ui-dev/msg01526.html — callers of an override include calls
  to the parent; no certainty that a static call never reaches the override.
- *callgraph/cha*, https://pkg.go.dev/golang.org/x/tools/go/callgraph/cha — class-hierarchy analysis and its spurious
  edges.
- Salis et al., *PyCG: Practical Call Graph Generation in Python*, ICSE 2021 — precision and recall of a careful
  static resolver.
- Huang et al., *Jarvis*, arXiv 2305.05949 — one wrong edge poisons a call chain.
- Feldthaus et al., *Efficient Construction of Approximate Call Graphs for JavaScript IDE Services*, ICSE 2013 —
  precision of name-keyed member resolution.
