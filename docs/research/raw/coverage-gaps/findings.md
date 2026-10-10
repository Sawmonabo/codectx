# Extraction and linking coverage: the eight facts a client derives that the index does not publish

Research of 2026-10-09 behind [ADR-0017](../../../adr/ADR-0017-extraction-and-linking-coverage.md). Read-only: nothing
in the repository was built, tested or changed. Repository claims cite `path:line` in the repository at commit e9c8799
(the code under `internal/` is unchanged on the branch these documents land on). Web claims cite a page fetched during
this research, by title and URL. Pages marked (self) were fetched or downloaded and checked by the researcher; the
rest were read by research helpers that returned a verbatim quote per claim.

## The question in plain words

A comparison of codectx with a client that consumes the index found twelve things that client derives from source
and codectx does not give it (an untracked comparison, numbered gaps 1 to 12). Four are settled elsewhere: gap 1 is
already published, gap 2 (the PHP language) is left out for now by the owner's decision, gap 6 (name-only call
candidates) is [ADR-0015](../../../adr/ADR-0015-open-call-sites-and-dispatch.md), and gap 7 (reading source outside
a session) is [ADR-0018](../../../adr/ADR-0018-read-source-outside-a-session.md). The question here is the other
eight: build them all before the one test pass, build some later, or build some in a different form.

Example of what is at stake (gap 8): an agent asks "who calls `handle`?". The MCP answer gives node IDs with no
source range and no call-site positions, and, because the client did not pass `relations`, it also mixes in data-flow
and containment edges (`internal/mcpserver/handlers.go:100-108`). The agent then needs one
`symbol` call per node and one `refs` call per callee to learn where anything is. An editor asking a language server
the same question gets every caller's range and every call-site range in one reply.

## What the evidence says, gap by gap

| Gap (plan node) | What mature tools do | Standard? | Precision / cost | Recommendation |
|---|---|---|---|---|
| **8** Ranges and call sites on `callers`/`callees`/`path`; MCP default `calls` | LSP call hierarchy: each item has `range` and `selectionRange`; an incoming call has `from` plus `fromRanges`, "the ranges at which the calls appear" (self). clangd fills one range per call site. SCIP occurrences, Kythe `ref/call` anchors and Glean `XRefLocation { target, source: RangeSpan }` all carry spans. An agent code-graph server (code-review-graph) returns file and line range per node and "one row per call site". LSP call hierarchy and Kythe `ref/call` are call-only by design. | **Yes, the baseline.** codectx is below every reference tool here, not just short of one client. | Exact (the spans are already stored: `StoredNode.Bytes`, `internal/storage/sqlite/query.go:178`). Small cost. | **Now, first.** It waits only on the shared declarations, not on stable identity. Return the full range and the name range per node, and every call-site range per `calls` relation, the way LSP's `fromRanges` does. |
| **9** `exports` for Python and C | Python: PEP 8 says modules "should explicitly declare the names in their public API … using the `__all__` attribute", and internal names use a leading underscore. pyright's library rules, griffe and Sphinx autodoc apply the same order: `__all__`, then underscore, then the `import A as A` re-export. pyright reads only the static `__all__` forms (`=`, `+=`, `.extend`, `.append`, `.remove`). C: functions have external linkage unless `static` (cppreference). universal-ctags marks `static` tags `file:`. Real shared-library export also depends on `-fvisibility` and on `visibility`/`dllexport` attributes, usually behind project macros (GCC manual, MSVC docs). | **Yes** for Python (shared rule set). Yes for C *linkage*; not for C *visibility*. | Python is exact for static `__all__`. A computed `__all__` cannot be read statically. C linkage is exact from the definition, but visibility macros need a preprocessor. Small cost: syntax of one file. | **Now, as planned**, with a stated form: Python uses static `__all__` when present, else the underscore rule, and the file's capability row says when `__all__` was not static. C `exports` means external linkage, documented as linkage, not shared-library visibility. |
| **10** JS function-valued properties `a.b = () => …`, `{ k: () => … }` | tree-sitter-javascript's own `queries/tags.scm` tags both as `@definition.function`: `assignment_expression` with an `identifier` or `member_expression` left and an arrow/function right, and `pair` with an arrow/function value (self, lines 63-74). GitHub search-based code navigation is built on these tags. universal-ctags tags `a.f7 = x => …` as a function scoped to `a`. stack-graphs binds pair keys as definitions. codectx's `ecmascript.scm:16` captures only `variable_declarator`. | **Yes.** The grammar's reference tags already do this. | Name only (`b`, `k`). String or computed keys are not matched, and reassignment gives two definitions (ADR-0014's content matching handles that). Small cost: two query patterns. | **Now, as planned.** Binding calls to the new declarations needs the declarations first. |
| **11** C object-like macro alias `#define NAME func` | Compiler-based tools (clangd, Kythe, CodeQL) get it free because the preprocessor runs first. Kythe records `ref/expands` from the name plus the call. Syntax-only tools (tree-sitter, cscope, ctags by default) miss it. ctags' opt-in expansion is same-file only and "about two times slower". Locally: the C worker records a `preproc_def` name only as a non-function name (`worker/link_c.go:211-214`). Function-like macros are already call targets (`link_c.go:22-28`). | **Yes** in compiler-based indexers. Missing in syntax-only ones. | Safe when the replacement is a single identifier naming a known function. Ambiguous under `#if` arms. A header-defined macro needs the include lookup the linker already does (`internal/provider/link/resolve_c.go:139-157`). Small cost. | **Now**, with a stated form: only a single-identifier replacement binds. One definition per preprocessor arm gives one `calls` per arm, with `SetSize`, as ADR-0015's several-target rule says. |
| **12** `requirements.txt` and `composer.json` dependencies | GitHub's dependency graph reads `requirements.txt` and `composer.json`/`composer.lock`. Dependabot supports pip and composer. OSV-Scanner reads `requirements.txt` and `composer.lock`, warning that manifests "declare version ranges". Format: pip's reference ("Each line of the requirements file indicates something to be installed", self) with `-r`/`-c` includes, `-e`, hashes, continuations and environment variables. PEP 508 covers the requirement line itself and excludes the file's options. Composer's schema defines `require`/`require-dev`, including `php` and `ext-*` platform entries. codectx's manifest provider has neither (`internal/provider/manifest/manifest.go:1-4`). | **Yes.** Every dependency scanner reads both. | `requirements.txt` has no full spec beyond pip's internals, so pick and state a subset. Without a lock file only direct dependencies are visible. Small cost. | **Now.** `requirements.txt`: PEP 508 lines, comments, continuations, and `-r`/`-c` followed with a cycle guard. Other pip options are recorded unresolved, as the provider already does for dynamic values (`manifest.go:5-8`). `composer.json` too, although PHP is deferred: the dependency facts do not need the PHP grammar, and PHP files are already stored and searchable. Lock files stay out, for parity with the other ecosystems (the provider reads none today). |
| **5** `imports` file → resolved file | C/C++: Kythe `ref/includes` ("an anchor that inlines the text of file F") and Glean (`#includes are xrefs to src.Files`). JS/TS: CodeQL `resolveImportedPath`, madge and dependency-cruiser. Python: grimp and import-linter build module import graphs. SCIP has no file-to-file field and marks imports with a role bit on symbols. stack-graphs is archived (self: `"archived": true`). | **Yes** for C includes. Yes in analyzers and dependency tools for JS/TS and Python. Navigation indexers link imports through symbols instead. | As precise as the linker's resolver: Python `sys.path` and namespace packages, package.json `exports` conditions and tsconfig `paths`, and include search paths without a compile database (clangd falls back to guessing). codectx's quoted-include match is "beside the file, or any file whose path ends in h" (`resolve_c.go:139-142`), so one include can match several files. Small cost: the linker already resolves these internally but publishes `calls` only (`internal/provider/link/provider.go:55-56`). | **Now**, with a stated form: publish only what the linker resolved. An include or specifier that matches several files gives one `imports` per file, with `SetSize`. Anything unresolved is ADR-0015's open import site. |
| **4** Interprocedural argument → parameter data flow | Joern's code property graph stores the *positions*, not the flow edge: "AST-children of CALL nodes have an argument index, that is used to match call-site arguments with callee parameters". Parameters are indexed with "index 0 … reserved for implicit self / this" (self), and `ARGUMENT_NAME` holds keyword names. Its engine does the step at query time (`argToMethods(arg).parameter`, depth limit `maxCallDepth: Int = 4`, self). CodeQL matches `ArgumentPosition` to `ParameterPosition` when the query runs, keeps "a one-level call context" (self), and uses flow summaries only for code outside the database. Python `*args`/`**kwargs` need synthetic positions. Infer, Semgrep Pro and FlowDroid use per-function summaries. SCIP, LSP and Glean have no argument flow (LSP only labels parameters with inlay hints; Glean marks it future work). No tool stores closed transitive chains. | **Yes** in data-flow and security engines. **No** in navigation indexers. The planned form (stored one-hop edge for bound sites, transitive by walking) goes one step past Joern, which stores only the indices. | Precise when it follows bound calls only, with no argument-to-parameter edge on unbound sites (plan B-T2). Because the plan publishes no return → call-result edge, a walk cannot enter one call and leave through another, the "unrealizable path" error IFDS guards against. Imprecision comes from Python keyword and `*args`/`**kwargs`, JS spread, and defaults. Cost is about one edge per argument at each bound call site, an estimate, not measured. Republication follows ADR-0014's per-site traces. | **Now, in this form:** store argument position (and keyword name, spread/variadic mark) on each `wire.Site` argument and parameter position (self = 0) on each parameter. Publish one `data_flows_to` per matched pair on bound sites only. A spread, or `*args`/`**kwargs` at the call, matches only up to the spread point and then to the callee's variadic parameter. A default parameter with no argument gets no edge. Transitive flow stays a query-time walk with no depth limit (no Joern-style k-limit, per the no-limits rule). Return-value flow is a follow-up, not part of gap 4. Needs stable identity: parameters are keyed by declaration identity and position (ADR-0014). |
| **3** Entry points and routes as facts | Security and API-inventory tools model routes per framework. CodeQL has shared concepts ("A data-flow node that sets up a route on a server", "Gets the URL pattern for this route, if it can be statically determined", self) with Flask, Django, Express, Spring and Go models. Checkmarx API Security lists endpoints from source. Semgrep has community route rules only. Navigation formats have no route kind: the SCIP symbol kinds, the LSP `SymbolKind` list and Kythe's node kinds. FastAPI's and springdoc's OpenAPI come from the running app, and Backstage API definitions are hand-written. An agent code-graph server (codebase-memory-mcp) makes "REST endpoints … first-class graph entities" (self). Program entry points are fixed by each language spec: Python `__main__`, Go `package main` + `func main`, and Java, whose candidate `main` "may be either a `static` or an instance method" with a `String[]` parameter or none, a form that "expanded significantly in Java SE 25" (JLS SE 25 §12.1.4, self). | **Yes in security tools; no in navigation formats.** Agent graph servers are starting to add it. | The route as written and its handler are exact. The *full URL* is often not, because prefixes live elsewhere: Flask blueprints, Django `include()`, FastAPI `include_router` prefix, Spring class-level `@RequestMapping`. CodeQL admits "relative paths of subrouters are not modeled" (Express.qll:351, self). Cost is M–L plus upkeep per framework. Program entry points are cheap. | **Now, in a split form:** A first part publishes the language-level facts every framework rule reads: decorators and annotations on their declaration (name as written, range, argument ranges), and program entry points (`__main__` guard, Go `main`, every Java `main` form JLS SE 25 §12.1.4 defines, including instance and no-parameter `main` and inherited candidates, and a shebang). A second part is a per-framework table over those facts and the call sites (Flask, FastAPI, Django, Express, Spring, Go `net/http`/mux) that publishes `endpoint` + `routes_to`. The route as written is always stored. The full path is published only when every prefix in the chain resolved statically; otherwise the endpoint says the prefix is unresolved. Never guess. |

Dependencies that hold in every option: binding calls to the new JavaScript declarations needs those declarations;
argument flow needs parameter identity, and the new declarations and endpoints need the descriptor path and
disambiguator ([ADR-0014](../../../adr/ADR-0014-stable-identity-incremental-linking.md)). The graph-answer ranges need
only the shared declarations. The manifest work touches only `internal/provider/manifest/**`, which never calls the
identity path: `rg "CanonicalKey|reconcile\."` over `internal/provider/manifest/` finds nothing.

## Options for the set

**(a) All eight before the test pass, exactly as first planned.** Every gap ships before the one test pass, so the
release proof runs on the whole product. Cost: the route work carries no stated precision rule, and the import, argument
flow and macro work carry no rule for several matches, keyword or spread arguments, or `#if` arms. The open questions
surface in the single review instead of in the shared declarations. Because the pieces run in parallel, this does not
lengthen the critical path. It does widen the one review and the one fix round.

**(b) The standard, small gaps now (8, 9, 10, 11, 12, 5); gaps 4 and 3 later.** This shrinks the extraction and linker
work. But the plan exists so that the test pass, proofs and benchmark run once. Deferring two gaps means a second test
pass, a second review, and either a second release proof or gaps landing after it. Gap 4 is not large under the form
above: the shared declarations already include the `wire.Site` argument fields. The real saving is gap 3's
per-framework tables.

**(c) All eight before the test pass, with the forms stated above (recommended).** Same work as (a). Gap 3 splits into
the language facts and a route table over them. The shared declarations fix the precision rules each gap needs, so
each piece is built to a written rule and the review checks against it. The extra declaration work is a few metadata
keys and test cases, nothing on the critical path.

**(d) Like (c), but the framework routes deferred.** It keeps the language-level entry points and decorators and
avoids the per-framework upkeep for now. The cost is the same second pass as (b) for the one piece most useful to
agents asking "what handles `/login`?". Choose (d) over (c) only to cap the review surface at the cost of a later test
pass.

## Recommendation: (c)

1. Each of the eight is standard in at least one class of mature tool (table above). Gap 8 is below the baseline
   every navigation tool meets (the LSP call hierarchy), so it is not optional for an agent-facing server.
2. "Build everything, then test once" is only true if nothing planned is pushed past the test pass. (b) and (d) buy a
   smaller review now with a second pass later. PHP was deferred as an additive language of several weeks; the route
   table is mid-sized once its form is fixed. And its `endpoint` nodes change what existing answers return: the kind
   filter and the reachability walk from endpoints. Deferring it would leave the `routes_to` declaration unused until
   after the release proof.
3. Six gaps are small and exact. The two larger ones become exact once their form is fixed: gap 4 by binding only
   resolved sites with positions as Joern and CodeQL do, and gap 3 by never guessing a full URL, which avoids the
   prefix errors CodeQL and Checkmarx document.
4. The order is set by what each piece waits on, not by sequence: the graph-answer ranges and the manifest readers
   wait only on the shared declarations; the JavaScript declarations, exports, routes, imports, argument flow and
   macros wait on stable identity; binding calls to the JavaScript declarations waits on those declarations.

## What the design fixes for each gap

- The shared declarations add:
  - the endpoint metadata keys: `framework`, `method`, `route` as written, `path` only when every prefix resolved,
    `prefix` = `resolved` | `unresolved`;
  - the decorator/annotation attribute shape on declarations: name as written, range, argument ranges;
  - on `wire.Site` arguments, a keyword name and a spread/variadic mark beside the ordered ranges;
  - on parameters, their index (self = 0), variadic and keyword-only marks;
  - the specification text for each of these.
- Routes split into the language facts (decorators/annotations, program entry points) and the per-framework route
  table. Its tests gain "a blueprint or `include_router` prefix that does not resolve gives `prefix: unresolved` and no
  `path`" (mutation: join an unresolved prefix as empty).
- `requirements.txt` follows `-r`/`-c` with a cycle guard, and the stated subset goes in the manifest docs. Its tests
  gain an include cycle (mutation: follow includes without a guard).
- Exports: the docs say C `exports` means external linkage. Its tests gain a computed `__all__` (mutation: treat it as
  empty).
- Imports: one `imports` per matched file, with `SetSize`. Its tests gain a quoted include matching two headers
  (mutation: keep the first match only).
- Argument flow: its tests gain a keyword argument, `*args`, a JS spread and an omitted default (mutation: match by
  position across a keyword argument).
- Macros: the tests gain a macro defined in two `#if` arms and a macro defined in an included header (mutation: bind
  only the first definition).
- Graph-answer ranges: the LSP shape, node full range and name range, and every call-site range per `calls` relation.
- Follow-ups, outside this question: return-value flow (callee return → call result), lock files (`composer.lock`,
  `pylock.toml`, and the existing ecosystems' locks), and C shared-library visibility through export macros.

## Sources

Repository (read during this research): `internal/mcpserver/handlers.go:100-108`, `internal/mcpserver/registry.go:73`,
`internal/model/facts.go:30,56-63`, `internal/provider/treesitter/lang/queries/ecmascript.scm:12-25`,
`internal/provider/treesitter/lang/queries/c.scm:29`, `internal/provider/treesitter/worker/link_c.go:20-30,211-214`,
`internal/provider/link/resolve_c.go:139-157`, `internal/provider/link/provider.go:55-56`,
`internal/provider/manifest/manifest.go:1-8`, `docs/providers-treesitter.md:196,338-341`.

Web:
- LSP 3.17, Call Hierarchy (self) — https://raw.githubusercontent.com/microsoft/language-server-protocol/gh-pages/_specifications/lsp/3.17/language/callHierarchy.md
- LSP 3.17, Inlay Hint — https://raw.githubusercontent.com/microsoft/language-server-protocol/gh-pages/_specifications/lsp/3.17/language/inlayHint.md
- clangd XRefs.cpp — https://github.com/llvm/llvm-project/blob/main/clang-tools-extra/clangd/XRefs.cpp
- SCIP protocol, scip.proto — https://github.com/sourcegraph/scip/blob/main/scip.proto
- Kythe schema — https://kythe.io/docs/schema/
- Glean codemarkup schema — https://github.com/facebookincubator/Glean/blob/main/glean/schema/source/codemarkup.types.angle
- Glean pp, cxx, python schemas — https://github.com/facebookincubator/Glean/tree/main/glean/schema/source
- CodeQL C/C++ `Call` — https://codeql.github.com/codeql-standard-libraries/cpp/semmle/code/cpp/exprs/Call.qll/type.Call$Call.html
- CodeQL C/C++ `MacroInvocation` — https://codeql.github.com/codeql-standard-libraries/cpp/semmle/code/cpp/Macro.qll/type.Macro$MacroInvocation.html
- code-review-graph — https://github.com/tirth8205/code-review-graph
- Serena — https://github.com/oraios/serena
- codebase-memory-mcp README (self) — https://github.com/DeusData/codebase-memory-mcp
- Anthropic, Writing effective tools for agents — https://www.anthropic.com/engineering/writing-tools-for-agents
- PEP 8, Public and internal interfaces — https://peps.python.org/pep-0008/
- Python tutorial, Modules — https://docs.python.org/3/tutorial/modules.html
- pyright, Typed libraries — https://github.com/microsoft/pyright/blob/main/docs/typed-libraries.md
- griffe, Public APIs — https://mkdocstrings.github.io/griffe/guide/users/recommendations/public-apis/
- Sphinx autodoc — https://www.sphinx-doc.org/en/master/usage/extensions/autodoc.html
- mypy stubgen — https://mypy.readthedocs.io/en/stable/stubgen.html
- cppreference, Storage duration and linkage (C) — https://en.cppreference.com/w/c/language/storage_duration
- GCC, Code Gen Options (visibility) — https://gcc.gnu.org/onlinedocs/gcc/Code-Gen-Options.html
- Microsoft, dllexport, dllimport — https://learn.microsoft.com/en-us/cpp/cpp/dllexport-dllimport
- universal-ctags manual — https://docs.ctags.io/en/latest/man/ctags.1.html
- universal-ctags C/C++ parser (macro expansion) — https://docs.ctags.io/en/latest/parser-cxx.html
- universal-ctags JavaScript units — https://github.com/universal-ctags/ctags/tree/master/Units/parser-javascript.r
- tree-sitter-javascript tags.scm (self) — https://github.com/tree-sitter/tree-sitter-javascript/blob/master/queries/tags.scm
- tree-sitter, Code Navigation — https://tree-sitter.github.io/tree-sitter/4-code-navigation.html
- GitHub Docs, Navigating code on GitHub — https://docs.github.com/en/repositories/working-with-files/using-files/navigating-code-on-github
- TypeScript navigationBar.ts (v5.9.3) — https://github.com/microsoft/TypeScript/blob/v5.9.3/src/services/navigationBar.ts
- stack-graphs (archived; self) — https://github.com/github/stack-graphs
- GCC cpp, Object-like Macros — https://gcc.gnu.org/onlinedocs/cpp/Object-like-Macros.html
- GCC cpp, Function-like Macros — https://gcc.gnu.org/onlinedocs/cpp/Function-like-Macros.html
- Clang Internals Manual — https://clang.llvm.org/docs/InternalsManual.html
- cscope man page — https://cscope.sourceforge.net/cscope_man_page.html
- tree-sitter-c — https://github.com/tree-sitter/tree-sitter-c
- Code property graph specification (self) — https://cpg.joern.io/
- Joern, interprocedural data flow (2024) — https://joern.io/blog/interproc-dataflow-2024/
- Joern Engine.scala (self) — https://github.com/joernio/joern/blob/master/dataflowengineoss/src/main/scala/io/joern/dataflowengineoss/queryengine/Engine.scala
- Joern, data-flow steps — https://docs.joern.io/cpgql/data-flow-steps/
- CodeQL data-flow library design (self) — https://github.com/github/codeql/blob/main/docs/ql-libraries/dataflow/dataflow.md
- CodeQL, Analyzing data flow in Python — https://codeql.github.com/docs/codeql-language-guides/analyzing-data-flow-in-python/
- CodeQL, Customizing library models for Java and Kotlin — https://github.com/github/codeql/blob/main/docs/codeql/codeql-language-guides/customizing-library-models-for-java-and-kotlin.rst
- CodeQL Python DataFlowDispatch.qll — https://github.com/github/codeql/blob/main/python/ql/lib/semmle/python/dataflow/new/internal/DataFlowDispatch.qll
- Reps, Horwitz, Sagiv, Precise interprocedural dataflow analysis via graph reachability (POPL 1995) — https://www.cs.cornell.edu/courses/cs711/2005fa/papers/rhs-popl95.pdf
- Infer, Separation logic and bi-abduction — https://fbinfer.com/docs/separation-logic-and-bi-abduction
- Infer, Pulse — https://fbinfer.com/docs/checker-pulse
- Semgrep, How we cut taint analysis time by 75% — https://semgrep.dev/blog/2026/how-we-cut-semgreps-taint-analysis-time-by-75-percent
- Semgrep Pro engine introduction — https://docs.semgrep.dev/semgrep-code/semgrep-pro-engine-intro
- FlowDroid — https://github.com/secure-software-engineering/FlowDroid
- GCC, Search Path — https://gcc.gnu.org/onlinedocs/cpp/Search-Path.html
- GCC, Preprocessor Options — https://gcc.gnu.org/onlinedocs/gcc/Preprocessor-Options.html
- clangd, Compile commands — https://clangd.llvm.org/design/compile-commands
- CodeQL JS `Import.resolveImportedPath` — https://codeql.github.com/codeql-standard-libraries/javascript/semmle/javascript/Modules.qll/predicate.Modules$Import$resolveImportedPath.0.html
- TypeScript, Modules reference — https://www.typescriptlang.org/docs/handbook/modules/reference.html
- Node.js, Packages — https://nodejs.org/api/packages.html
- madge — https://github.com/pahen/madge
- dependency-cruiser — https://github.com/sverweij/dependency-cruiser
- grimp — https://github.com/seddonym/grimp
- import-linter — https://github.com/seddonym/import-linter
- CodeQL Python `Module` — https://codeql.github.com/codeql-standard-libraries/python/semmle/python/Module.qll/module.Module.html
- Python, The import system — https://docs.python.org/3/reference/import.html
- CodeQL Python Concepts.qll (self) — https://github.com/github/codeql/blob/main/python/ql/lib/semmle/python/Concepts.qll
- CodeQL Flask.qll — https://github.com/github/codeql/blob/main/python/ql/lib/semmle/python/frameworks/Flask.qll
- CodeQL Django.qll — https://github.com/github/codeql/blob/main/python/ql/lib/semmle/python/frameworks/Django.qll
- CodeQL Express.qll (self) — https://github.com/github/codeql/blob/main/javascript/ql/lib/semmle/javascript/frameworks/Express.qll
- CodeQL SpringController.qll — https://github.com/github/codeql/blob/main/java/ql/lib/semmle/code/java/frameworks/spring/SpringController.qll
- CodeQL Go HTTP concepts — https://github.com/github/codeql/blob/main/go/ql/lib/semmle/go/concepts/HTTP.qll
- Checkmarx, API Security language and framework support — https://docs.checkmarx.com/en/34965-190118-api-security-language-and-framework-support.html
- elttam semgrep-rules — https://github.com/elttam/semgrep-rules
- route-detect — https://github.com/mschwager/route-detect
- vscode-languageserver-node types (SymbolKind) — https://github.com/microsoft/vscode-languageserver-node/blob/main/types/src/main.ts
- Sourcegraph, Precise code navigation — https://sourcegraph.com/docs/code-navigation/precise-code-navigation
- FastAPI, Extending OpenAPI — https://fastapi.tiangolo.com/how-to/extending-openapi/
- FastAPI, Bigger applications — https://fastapi.tiangolo.com/tutorial/bigger-applications/
- springdoc-openapi — https://springdoc.org/
- Backstage, Descriptor format — https://backstage.io/docs/features/software-catalog/descriptor-format/
- Flask, Blueprints — https://flask.palletsprojects.com/en/stable/blueprints/
- Django, URL dispatcher — https://docs.djangoproject.com/en/5.2/topics/http/urls/
- Spring, Mapping requests — https://docs.spring.io/spring-framework/reference/web/webmvc/mvc-controller/ann-requestmapping.html
- Python, `__main__` — https://docs.python.org/3/library/__main__.html
- Python packaging, Entry points — https://packaging.python.org/en/latest/specifications/entry-points/
- The Go Programming Language Specification — https://go.dev/ref/spec
- Java Language Specification SE 25, §12.1.4 Invoke a main Method (self) — https://docs.oracle.com/javase/specs/jls/se25/html/jls-12.html
- pip, Requirements file format (self) — https://pip.pypa.io/en/stable/reference/requirements-file-format/
- pip, Requirement specifiers — https://pip.pypa.io/en/stable/reference/requirement-specifiers/
- PEP 508 — https://peps.python.org/pep-0508/
- pylock.toml specification — https://packaging.python.org/en/latest/specifications/pylock-toml/
- Composer, The composer.json schema — https://getcomposer.org/doc/04-schema.md
- Composer, Basic usage — https://getcomposer.org/doc/01-basic-usage.md
- GitHub, Dependency graph supported package ecosystems — https://docs.github.com/en/code-security/supply-chain-security/understanding-your-software-supply-chain/dependency-graph-supported-package-ecosystems
- GitHub, Dependabot supported ecosystems — https://docs.github.com/en/code-security/dependabot/ecosystems-supported-by-dependabot/supported-ecosystems-and-repositories
- OSV-Scanner, Supported languages and lockfiles — https://google.github.io/osv-scanner/supported-languages-and-lockfiles/
- OSV-Scanner issue 34 — https://github.com/google/osv-scanner/issues/34
- Semgrep Supply Chain requirements — https://docs.semgrep.dev/semgrep-supply-chain/requirements-and-feature-support
- pip-requirements-parser — https://github.com/aboutcode-org/pip-requirements-parser
