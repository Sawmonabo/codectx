# Extraction and linking coverage: what mature tools publish that the index does not, and in what form

Research note for the codectx product owner, behind
[ADR-0017](../adr/ADR-0017-extraction-and-linking-coverage.md). Research date 2026-10-09. The full gap-by-gap table,
the options and every source are in [raw/coverage-gaps/findings.md](raw/coverage-gaps/findings.md); sources are cited
here as [n] from §5.

## 1. The question

A client that consumes the index re-derives eight kinds of fact from source because the index does not publish them:
ranges and call sites on graph answers; Python and C exports; JavaScript function-valued properties; C macros aliasing
a function; `requirements.txt` and `composer.json` dependencies; file-to-file imports; argument-to-parameter flow; and
program entry points and routes. Is each standard in mature tools, how precise can it be, and should all eight be built
now and in what form?

## 2. What the field does

- **Ranges and call sites.** The editor protocol's call hierarchy returns each item's range and name range, and for an
  incoming call every call-site range [1]; the cross-tool index formats and code graphs all carry spans on references
  and calls [2][3]. This is the baseline every navigation tool meets.
- **Python and C exports.** The Python style guide asks modules to declare their public API in `__all__` and mark
  internal names with an underscore, and type checkers and documentation tools apply that order, reading only the
  static `__all__` forms [4][5]. C functions have external linkage unless `static` [6]; shared-library visibility also
  depends on compiler flags and attribute macros [7].
- **Function-valued properties.** The JavaScript grammar's own reference tags query tags `a.b = () => …` and
  `{ k: () => … }` as function definitions, and a code-host's search-based navigation is built on those tags [8].
- **Object-like macro aliases.** Compiler-based indexers see `#define NAME func` for free, since the preprocessor runs
  first, and record the expansion as a reference [9]; syntax-only tools miss it.
- **Python and PHP manifests.** Dependency graphs, update bots and vulnerability scanners all read `requirements.txt`
  and `composer.json` [10][11]; the file formats are pip's reference with `-r`/`-c` includes and the requirement
  specifier standard [12][13], and Composer's schema [14].
- **File-to-file imports.** Include graphs are first-class in C/C++ indexers; JavaScript and Python dependency tools
  build module import graphs [15][16]. Navigation formats link imports through symbols instead.
- **Argument-to-parameter flow.** Data-flow engines store argument and parameter positions (index 0 for the receiver,
  keyword names beside them) and match them when the query runs [17][18]; navigation formats have no argument flow.
  No tool stores closed transitive chains. Without a return edge, a walk cannot leave through a different call than it
  entered, the unrealizable path that interprocedural analysis guards against [19].
- **Entry points and routes.** Security and API-inventory engines model routes per framework, with a URL pattern "if
  it can be statically determined", and say that subrouter prefixes are not modelled [20][21]; navigation formats have
  no route kind. Program entry points are fixed by each language: a `__main__` guard, Go `main`, and the Java `main`
  forms, which grew in Java SE 25 to include instance and parameterless forms [22].

## 3. Findings

Each of the eight is standard in at least one class of mature tool, and ranges and call sites are below the baseline
of every navigation tool. Six are small and exact. The two larger ones become exact once their form is fixed:

| Gap | Exact form |
|---|---|
| Ranges and call sites | each node's full range and name range; every call-site range per `calls`; MCP call walks default to `calls` |
| Python and C exports | static `__all__`, else the underscore rule, and the capability row says when `__all__` was not static; C means external linkage |
| Function-valued properties | the two shapes as declarations, named by the property; string and computed keys excluded |
| Macro aliases | a single-identifier replacement naming a known function binds; one `calls` per `#if` arm with the set size |
| Manifests | requirement lines, comments, continuations, `-r`/`-c` with a cycle guard; other options unresolved; `composer.json` `require`/`require-dev`; no lock files |
| File-to-file imports | only what the linker resolved; one `imports` per matched file with the set size |
| Argument flow | positions, keyword names and spread marks; one `data_flows_to` per matched pair on bound sites only; no return edge |
| Entry points and routes | decorators and entry points as language facts; a per-framework route table; the route as written always; the full path only when every prefix resolved, otherwise `prefix: unresolved` |

Building all eight before the one test pass, with these forms fixed in the shared declarations, keeps a single test
pass and review; deferring argument flow or routes buys a smaller review now for a second pass later, and routes change
what existing answers return.

## 4. Not tested hands-on

The findings rest on documentation and source read during the research; no tool was run. Follow-ups outside the
question: return-value flow, lock files, and C export macros.

## 5. Sources

1. *Call Hierarchy*, LSP 3.17, https://raw.githubusercontent.com/microsoft/language-server-protocol/gh-pages/_specifications/lsp/3.17/language/callHierarchy.md
2. *scip.proto*, https://github.com/sourcegraph/scip/blob/main/scip.proto
3. *Kythe schema*, https://kythe.io/docs/schema/
4. *PEP 8*, https://peps.python.org/pep-0008/
5. *Typed libraries*, https://github.com/microsoft/pyright/blob/main/docs/typed-libraries.md
6. *Storage duration and linkage*, https://en.cppreference.com/w/c/language/storage_duration
7. *Code Gen Options*, https://gcc.gnu.org/onlinedocs/gcc/Code-Gen-Options.html
8. *tags.scm*, https://github.com/tree-sitter/tree-sitter-javascript/blob/master/queries/tags.scm; *Navigating code on
   GitHub*, https://docs.github.com/en/repositories/working-with-files/using-files/navigating-code-on-github
9. *MacroInvocation*, https://codeql.github.com/codeql-standard-libraries/cpp/semmle/code/cpp/Macro.qll/type.Macro$MacroInvocation.html
10. *Dependency graph supported package ecosystems*, https://docs.github.com/en/code-security/supply-chain-security/understanding-your-software-supply-chain/dependency-graph-supported-package-ecosystems
11. *Supported languages and lockfiles*, https://google.github.io/osv-scanner/supported-languages-and-lockfiles/
12. *Requirements File Format*, https://pip.pypa.io/en/stable/reference/requirements-file-format/
13. *PEP 508*, https://peps.python.org/pep-0508/
14. *The composer.json schema*, https://getcomposer.org/doc/04-schema.md
15. *Glean schema sources*, https://github.com/facebookincubator/Glean/tree/main/glean/schema/source
16. *dependency-cruiser*, https://github.com/sverweij/dependency-cruiser; *grimp*, https://github.com/seddonym/grimp
17. *Code property graph specification*, https://cpg.joern.io/
18. *Data flow library design*, https://github.com/github/codeql/blob/main/docs/ql-libraries/dataflow/dataflow.md
19. Reps, Horwitz, Sagiv, *Precise interprocedural dataflow analysis via graph reachability*, POPL 1995,
    https://www.cs.cornell.edu/courses/cs711/2005fa/papers/rhs-popl95.pdf
20. *Concepts.qll*, https://github.com/github/codeql/blob/main/python/ql/lib/semmle/python/Concepts.qll
21. *Express.qll*, https://github.com/github/codeql/blob/main/javascript/ql/lib/semmle/javascript/frameworks/Express.qll
22. *The Java Language Specification, Java SE 25*, §12.1.4, https://docs.oracle.com/javase/specs/jls/se25/html/jls-12.html
