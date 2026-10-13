# ADR-0017: Eight facts a client derives from source are published by the index, each in a stated exact form

## Status

Accepted, 2026-10-09, by the owner's ruling: all eight, each in the form below. Not yet implemented. The survey behind
it is [research 26](../research/26-extraction-and-linking-coverage.md).

## Context

- **A client derives what the index leaves out.** A client that consumes the index re-derives from source twelve
  kinds of fact the index does not give it. Four are settled elsewhere: one is already published, the PHP language is
  left out for now by the owner's decision, name-only call candidates are
  [ADR-0015](ADR-0015-open-call-sites-and-dispatch.md), and reading source outside a session is
  [ADR-0018](ADR-0018-read-source-outside-a-session.md). This record covers the other eight.
- **Worked example.** An agent asks "who calls `handle`?". The MCP answer gives node IDs with no source range and no
  call-site positions, and with `relations` left empty it walks the default relation set
  (`internal/graph/traverse.go:1075-1077`), so data-flow and containment edges come back beside the calls, while the
  CLI pins `calls` (`internal/cli/query.go:151-167`). The agent then needs one symbol lookup per node and one
  references lookup per callee to learn where anything is. A language server answers the same question with every
  caller's range and every call-site range in one reply.
- **What the code publishes now.** The structural provider exports for Go, Rust, Java and JavaScript/TypeScript only
  (`docs/providers-treesitter.md:196`). The ECMAScript query captures a function only through a variable declarator
  (`internal/provider/treesitter/lang/queries/ecmascript.scm:16`). A C `#define NAME` is captured only as a constant
  (`internal/provider/treesitter/lang/queries/c.scm:29`). The manifest provider reads `go.mod`, `go.work`,
  `package.json`, `Cargo.toml`, `pyproject.toml` and `pom.xml` (`internal/provider/manifest/manifest.go:1-8`). The
  linker resolves imports internally and declares `calls` alone (`internal/provider/link/provider.go:55`). The
  `endpoint` node kind exists (`internal/model/facts.go:30`) and nothing publishes it.
- **The requirement.** Complete answers by default; every fact exact, or labelled for what it is; never a guess
  presented as a fact; no fixed counts on generated data.

## Decision

### 1. Graph answers carry ranges and call sites, and MCP call walks default to `calls`

- Every node in a `callers`, `callees` or `path` answer carries its full range and its name range, and every `calls`
  relation carries the range of each call site, the shape of the language server protocol's call hierarchy
  (`range`, `selectionRange`, `fromRanges`) [S1]. The spans are already stored
  (`internal/storage/sqlite/query.go:178`).
- `codectx_callers` and `codectx_callees` with `relations` empty walk `calls`, as the CLI does.

### 2. Python and C publish `exports`

- **Python:** the static `__all__` when the module has one (`=`, `+=`, `.extend`, `.append`, `.remove` of literal
  names), otherwise every top-level name without a leading underscore [S2]. A module whose `__all__` is computed falls
  back to the underscore rule, and the file's capability row says that `__all__` was not static.
- **C:** every definition with external linkage, that is, not `static` [S3]. `exports` means linkage, documented as
  such, not shared-library visibility.
- **C and C++ visibility as declared in source:** a declaration's own attributes (`visibility("…")`, `__declspec`
  `dllexport`/`dllimport`) give its `visibility` metadata; a macro among its specifiers is read by the linker through
  the definitions its file sees: those of the file and of every header in its include closure, each `#include`
  resolved by decision 4's include lookup and each header read once [S22], following one-identifier replacements, and
  published as
  a `references` to each definition with the visibility it states, one per `#if` arm with the set size. It is never
  claimed as the binary's export table: `-fvisibility` and the toolchain's export lists decide that, and no attribute
  publishes no visibility [S4]. No macro name is known in advance; the project's own `#define` is read.

### 3. JavaScript and TypeScript function-valued properties are declarations

`a.b = () => …` (an assignment whose left side is an identifier or member expression and whose right side is an
arrow or function expression) and `{ k: () => … }` (a pair with such a value) are function declarations named `b` and
`k`, the shapes the grammar's own reference tags query defines as function definitions [S5]. Their descriptor paths are
`a.b` and the object binding's path followed by `k`, with ADR-0014's disambiguator; a property assigned twice is two
declarations, told apart by content matching. String and computed keys are not declarations. Calls bind to these
declarations as to any other.

### 4. A C object-like macro naming a function binds calls through it

`#define NAME func`, where the replacement is a single identifier naming a known function, binds a call of `NAME` to
`func`, as a preprocessor-based indexer sees it [S6]. A macro defined in several `#if` arms binds one `calls` per
definition, each with `set_size` the number of definitions (ADR-0015's several-target rule). A macro defined in an
header in the including file's include closure binds: the file's directives and those of every header they reach,
each resolved by the include lookup the linker already performs (`internal/provider/link/resolve_c.go` `includes`),
each header read once, as an include guard makes a second inclusion add nothing [S22]. The same closure decides which
functions of internal linkage a call may bind: those of the file and of a header the closure reaches, and, only when
the closure holds a directive the unit cannot follow (an unfolded macro, a quoted path no member file answers), those
of other headers read with the functions of external linkage (`internal/provider/link/include_c.go`). Any other
replacement binds nothing.

### 5. `requirements.txt` and `composer.json` produce dependency nodes

- **`requirements.txt`:** the requirement lines of the dependency specification standard (name, extras, version
  specifiers, markers), comments and line continuations, and `-r`/`-c` includes, followed with a guard so an include
  cycle ends [S7][S8]. Every other pip option is recorded as unresolved, as the provider already records a value its
  format leaves dynamic. The supported subset is stated in the manifest documentation.
- **`composer.json`:** `require` and `require-dev`, the platform entries `php` and `ext-*` included [S9]. It is read
  although PHP code is not indexed: the dependency facts need no PHP grammar.
- **Lock files** (`package-lock.json` and `npm-shrinkwrap.json`, `pnpm-lock.yaml`, `yarn.lock`, `bun.lock`,
  `Cargo.lock`, `poetry.lock`, `uv.lock`, `Pipfile.lock`, `pylock.toml` and `pylock.<name>.toml`, `pdm.lock`,
  `gradle.lockfile` and `buildscript-gradle.lockfile`) are read, each by its published format,
  as a stream: JSON tokens with one package decoded at a time, TOML one package table at a time, the text formats
  line by line [S20][S21]. The lock defines a `configuration` node; each pinned package is a `depends_on`
  occurrence from it to the dependency node a manifest's requirement of the same name reaches, with
  `resolved_version`, the package's integrity where the format has one, and `set_size` the number of versions the
  lock pins for the name; each package's dependencies are `depends_on` occurrences between dependency nodes, with
  the resolved version where the lock states it. A package the lock installs under an alias is the package's own
  node, the alias in the detail. These are evidence detail, because a dependency node is shared by
  every manifest and lock that names it. `composer.lock` is PHP's and is left out with PHP.

### 6. The linker publishes file-to-file `imports` for what it resolved

The linker publishes `imports` from a file's module to each file module an import resolved to: Python modules,
JavaScript and TypeScript specifiers, and quoted C and C++ includes. An import that matches several files (a quoted
include matched beside the file and by path suffix) publishes one `imports` per matched file, each with `set_size`.
An import that does not resolve publishes nothing here; a non-constant one is ADR-0015's open import site.

### 7. Arguments flow into the parameters of the callee a site is bound to, and back through its return summary

- The worker records, on each call site's arguments, their positions, a keyword name and a spread or variadic mark;
  on each parameter, its index (the receiver or `self` is 0) and its variadic and keyword-only marks.
- On a bound site only, the linker publishes one `data_flows_to` from each argument's value to the parameter it
  matches: by position up to the first keyword argument, then by keyword name. A spread argument, or `*args` or
  `**kwargs` at the call, matches up to the spread point and then the callee's variadic parameter. A parameter left at
  its default gets no edge. An unbound site publishes no argument flow.
- Transitive flow is a walk at query time, with no depth limit and no call stack. A plain edge from a callee's
  return to every call of it would let a walk enter one call and leave through another, the unrealizable-path error
  of interprocedural analysis [S10]. Return values come back by labelled edges instead [S10][S12], and the walk
  keeps its paths realizable by the two-phase rule of interprocedural slicing [S10]: a path that has taken an
  `argument` edge never takes a `return` edge.
  - **Return summary.** From the worker's intraprocedural def-use and control dependence, each parameter whose
    value reaches a return of its callable, or decides which return runs, is marked. At each bound site whose
    value is written into variables, each argument bound to such a parameter publishes one `data_flows_to` from
    each variable it reads to each of those variables, labelled `summary:` and the parameter's position. The walk
    takes it without entering the callee, and it carries every effect the parameter has on the result.
  - **Return node.** Each analysed callable has a return node, a node of its own kind (`return`) at the whole
    callable with no name range, as the formal return is an unnamed node type of its own in a code property graph
    [S11], keyed by the callable's identity, into
    which every value a returning node reads, or a node deciding whether it runs reads, flows as an ordinary
    reaching definition, a parameter's value included. Each bound site publishes the callee's return node flowing
    into the variables the call's value is written into, and into the caller's own return node when the caller
    returns the call's value (`return f()`), labelled `return`, so a factory behind a wrapper reaches the
    wrapper's callers. A path that starts inside the callee leaves through every call of it, a partially balanced
    path; a path that entered through an argument does not leave, since its effect is the summary's. A value
    created in the callee whose return a parameter decides (`k = 1; if p: return k`) still reaches every caller's
    result.
  - A call whose value another call consumes first (`g(f())`, `f().m()`) has no result destination and publishes
    neither. A constructor that returns no value (a Python `__init__`, a Java or C++ constructor) has no return
    node.
- Parameters and return nodes are keyed by their callable's identity (ADR-0014), so another file's flow into a
  parameter or out of a return node survives an edit of the callee's body; a body edit that changes whether a
  parameter reaches the return changes the callee's interface entry the bound sites read, and re-runs those sites.

This stores one hop where data-flow engines store the argument and parameter positions and match them at query
time [S11][S12]; the stored hop is the same match made once per bound site. The summary is the RHS summary edge
from an actual-in to an actual-out of the site [S10]; the return node stands where a code property graph's method
return does [S11]. With the edge's role in its evidence detail, the walk needs one flag per path, not the
call-site matching a query-time library keeps [S12].

### 8. Program entry points, decorators and routes are facts; a route's full path only when it resolves

- **Decorators and annotations** are published on their declaration: name as written, range, and the range of each
  argument.
- **Program entry points** are `endpoint` nodes with `routes_to` to the entry function: a Python `__main__` guard; Go
  `package main` with `func main`; every Java `main` form the language specification defines, instance and
  parameterless forms and inherited candidates included [S13]; a script with a shebang; and an application object a
  module binds (a Flask, FastAPI, Starlette or Express application, Django's WSGI or ASGI application), found by its
  constructor call and never by the file's name, since neither PEP 3333 nor ASGI names a file.
- **Framework routes** come from a per-framework table over those facts and the call sites, for Flask, FastAPI,
  Django, Express, Spring and Go `net/http` with its common routers. Each publishes an `endpoint` node with
  `routes_to` to the handler and the metadata `framework`, `method`, `route` as written, and `prefix`: `resolved` or
  `unresolved`. `path`, the full URL path, is present only when every prefix in the chain (a blueprint, an
  `include_router`, a router mount, a class-level mapping) resolved statically. An unresolved prefix is never joined
  as empty and never guessed.
- An endpoint's identity is its file, kind and descriptor (the method and the route as written, or `__main__` /
  `main`), never its range.
- **Prefixes across files** are joined by the linker, never by the per-file provider. Each Python file's link table
  carries its router objects, the routes registered on each (the route's literal value below the object), and its
  mounts of objects it does not build (`include_router`, `register_blueprint`, Django `include()`). The linker resolves
  a mount's argument through the import to the binding it names, joins the prefixes (a Flask registration's
  `url_prefix=` replaces the blueprint's own), and walks down from each mount anchored at an application root,
  publishing one `endpoint` per route reached on the mount's file, at its full path. A Django urlconf is a root only
  when the settings module some file names in `DJANGO_SETTINGS_MODULE` names it in `ROOT_URLCONF` [S15]. A route
  several mounts reach is published once per path, each with the set size; a hop whose prefix is not a literal
  leaves the route `unresolved` with no path; the walk ends where a binding repeats on its way and is bounded by the
  mounts the unit holds. The linker is where this happens because only it sees every file of the unit and already
  resolves imports to bindings; the per-file provider would need every other file's routers to do it.
- **Declared entry points** are the ones a project's manifest declares: `[project.scripts]`, `[project.gui-scripts]`
  and `[project.entry-points.*]` of `pyproject.toml` [S16], `bin` and `main` of `package.json` [S17], `[[bin]]` and the
  `src/bin` defaults of `Cargo.toml` [S18]. The linker reads the manifest at the unit's root and publishes an
  `endpoint` at the declaration each names (the function of an object reference, the module of a JavaScript file,
  a Rust binary's `fn main`), because resolving a dotted module or a file path to a declaration needs the whole
  unit's module index.
- **Request inputs** are facts on the reading expression: a `variable` node with the framework, the part of the
  request read and its literal key, which the enclosing function `reads`, and a read assigned to a variable flows to
  it, so an alias reaches its uses through def-use. This is the shape of a remote-flow source with its source type
  [S19] and of a tag on a graph node [S11]: a node a query starts from, not a relation of its own. Each table is
  gated on the framework's import; a read it does not model publishes nothing. A read whose source another file
  decides (a FastAPI parameter typed by a class of another module, a Spring parameter left to the catch-all, which
  the project's own argument resolvers precede) is left to the linker in the link table, which publishes the same
  node shape once the unit's index decides it, and nothing when it cannot.

**Alternatives.** Publishing the guessed full path: prefixes live in other files and are often computed, and the
best-known route models say so themselves ("relative paths of subrouters are not modeled") [S14]. Routes from the
running application's OpenAPI document: needs the application to run, which the index never does. Joining
cross-file prefixes in the per-file provider: every other file's routers would have to be read into each file's run.
A request input as a relation from the function to a synthetic request node: one node per request would join every
handler's reads into one node, and the expression's range, key and source would ride on edge evidence that a query
cannot filter on.

### 9. All eight are one change

The eight facts are designed, built and verified together, each in the form above.

**Alternatives.** The six small facts now, argument flow and routes later: the two larger facts are exact once their
form is fixed, and splitting them off means a second round of verification for the same answers. Routes later alone:
endpoint nodes change what existing answers return (the kind filter, the walk from endpoints), so that change would
reach those answers without the verification the rest receives.

### 10. Proof

- One `callers` call returns every node's range and every call site's range, with no follow-up symbol or references
  call; an MCP `callers` call with empty `relations` returns no containment or data-flow edge.
- A Python module with a literal `__all__` exports exactly those names; a computed `__all__` falls back to the
  underscore rule and says so (mutation: treat it as empty); a C `static` function is not exported.
- `a.b = () => …` and `{ k: () => … }` are declarations (mutation: capture only the variable declarator), and
  `obj.run = () => …; obj.run()` binds.
- `#define LOG log_impl` then `LOG()` calls `log_impl`; a macro defined in two `#if` arms binds both, and one defined
  in an included header binds (mutation: bind only the first definition).
- A `requirements.txt` with markers, extras, comments and an `-r` include cycle yields exactly its packages and ends
  (mutation: follow includes without a guard).
- A JavaScript relative import and a Python `from a import b` give file-to-file `imports`; a quoted include matching
  two headers gives one `imports` per header, `set_size` 2 (mutation: keep the first match only).
- `f(x)` bound to `g(p)` gives `x → p`; an unbound site gives none (mutation: bind by arity alone); a keyword
  argument, `*args`, a JavaScript spread and an omitted default each map to the right parameter (mutation: match by
  position across a keyword argument). `a = ident(x)` with `ident` returning its parameter gives `x → a` labelled
  `summary:1`, and `b = make()` gives `make`'s return node `→ b`, in every family; in `k = 1; if p: return k` called
  at two sites, k flows into the return node and on to both results, each edge with one role (mutation: leave out a
  value a parameter decides), and `print(make())` gives no return flow (mutation: take the innermost node's
  definitions past another call).
- `@app.route("/x")` yields one endpoint with `routes_to` its handler, and a decorator that is not a route yields none
  (mutation: any decorator is a route); a blueprint or `include_router` prefix that does not resolve gives
  `prefix: unresolved` and no `path` (mutation: join an unresolved prefix as empty); the kind filter returns
  endpoints, and a `callees` walk from them returns what they reach.

## Consequences

- Graph answers become self-locating, so an agent learns where every caller and call site is from one call.
- New facts per file: exports for two languages, JavaScript property declarations, macro-bound calls, file-to-file
  imports, one argument-flow edge per argument at each bound site (an estimate, not measured), decorator facts and
  endpoints. Storage is measured in the scale proofs.
- Endpoint and argument-flow identities rest on ADR-0014's descriptor paths and parameter keys.
- New facts per bound site whose value is used: one return flow, and one summary flow per argument bound to a
  returning parameter; one return node per analysed callable that a used call reaches or whose returning nodes read
  a variable.
- Follow-ups outside this record: lock files of the ecosystems decision 5 does not list.

## Sources

- [S1] *Call Hierarchy*, LSP 3.17, https://raw.githubusercontent.com/microsoft/language-server-protocol/gh-pages/_specifications/lsp/3.17/language/callHierarchy.md
  — `range`, `selectionRange` and `fromRanges`, "the ranges at which the calls appear".
- [S2] *PEP 8, Public and internal interfaces*, https://peps.python.org/pep-0008/ — `__all__` and the leading
  underscore; and *Typed libraries*, https://github.com/microsoft/pyright/blob/main/docs/typed-libraries.md — the
  static `__all__` forms.
- [S3] *Storage duration and linkage*, https://en.cppreference.com/w/c/language/storage_duration — external linkage
  unless `static`.
- [S4] *Code Gen Options*, https://gcc.gnu.org/onlinedocs/gcc/Code-Gen-Options.html — `-fvisibility`; and
  *dllexport, dllimport*, https://learn.microsoft.com/en-us/cpp/cpp/dllexport-dllimport; *Common Attributes*,
  https://gcc.gnu.org/onlinedocs/gcc/Common-Attributes.html — "four supported visibility_type values: `default`,
  `hidden`, `protected`, or `internal`"; and *Attribute Syntax*, https://gcc.gnu.org/onlinedocs/gcc/Attribute-Syntax.html
  — attribute names may be written with `__` before and after.
- [S5] *tags.scm*, https://github.com/tree-sitter/tree-sitter-javascript/blob/master/queries/tags.scm — assignment and
  pair patterns tagged as function definitions.
- [S6] *Object-like Macros*, https://gcc.gnu.org/onlinedocs/cpp/Object-like-Macros.html; and *Kythe schema*,
  https://kythe.io/docs/schema/ — `ref/expands` from a macro use.
- [S7] *Requirements File Format*, https://pip.pypa.io/en/stable/reference/requirements-file-format/ — one
  requirement per line, `-r` and `-c` includes.
- [S8] *PEP 508*, https://peps.python.org/pep-0508/ — the requirement specifier.
- [S9] *The composer.json schema*, https://getcomposer.org/doc/04-schema.md — `require`, `require-dev`, platform
  packages.
- [S10] Reps, Horwitz, Sagiv, *Precise interprocedural dataflow analysis via graph reachability*, POPL 1995,
  https://www.cs.cornell.edu/courses/cs711/2005fa/papers/rhs-popl95.pdf — realizable paths.
- [S11] *Code property graph specification*, https://cpg.joern.io/ — argument indices matched to parameter indices,
  index 0 for the receiver; METHOD_RETURN, "an (unnamed) formal method return parameter", a node type of its own
  with no NAME property, whose CODE "MAY be set freely, e.g., to the constant RET".
- [S12] *Data flow library design*, https://github.com/github/codeql/blob/main/docs/ql-libraries/dataflow/dataflow.md
  — argument and parameter positions matched when the query runs.
- [S13] *The Java Language Specification, Java SE 25*, §12.1.4, https://docs.oracle.com/javase/specs/jls/se25/html/jls-12.html
  — the `main` method forms.
- [S14] *Express.qll*, https://github.com/github/codeql/blob/main/javascript/ql/lib/semmle/javascript/frameworks/Express.qll
  — "relative paths of subrouters are not modeled".
- [S15] *Django settings: ROOT_URLCONF; Designating the settings*, https://docs.djangoproject.com/en/stable/ref/settings/#root-urlconf
  and https://docs.djangoproject.com/en/stable/topics/settings/#designating-the-settings — the urlconf a request
  starts from, named by the settings module `DJANGO_SETTINGS_MODULE` names.
- [S16] *Entry points specification*, https://packaging.python.org/en/latest/specifications/entry-points/ — an object
  reference `module:attr`, console and GUI scripts.
- [S17] *package.json*, https://docs.npmjs.com/cli/v10/configuring-npm/package-json — `bin` and `main`.
- [S18] *Cargo targets*, https://doc.rust-lang.org/cargo/reference/cargo-targets.html — `[[bin]]` and `src/bin`.
- [S19] *RemoteFlowSource*, https://codeql.github.com/codeql-standard-libraries/python/semmle/python/dataflow/new/RemoteFlowSources.qll/type.RemoteFlowSources$RemoteFlowSource.html
  — a source node with its source type.
- [S20] *OSV-Scanner, supported lockfiles*, https://google.github.io/osv-scanner/supported-languages-and-lockfiles/ —
  "prefer a resolved lockfile"; the lock formats per ecosystem.
- [S21] *pylock.toml specification* (PEP 751), https://packaging.python.org/en/latest/specifications/pylock-toml/ —
  `[[packages]]` with `name`, `version`, `dependencies` and hashed sources.
- [S22] *The C Preprocessor*: *Search Path*, https://gcc.gnu.org/onlinedocs/cpp/Search-Path.html — a quoted include
  is searched "first relative to the directory of the current file"; *Once-Only Headers*,
  https://gcc.gnu.org/onlinedocs/cpp/Once-Only-Headers.html — a guarded header included again: "The preprocessor
  will skip over the entire contents of the file".
- [research 26](../research/26-extraction-and-linking-coverage.md) and
  [raw/coverage-gaps](../research/raw/coverage-gaps/findings.md) — the gap-by-gap survey, the options and the full
  source list.
