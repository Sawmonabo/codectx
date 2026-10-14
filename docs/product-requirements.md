# Product requirements

What codectx is for, who uses it, the requirements every part of it meets, and the capabilities it provides. Other
documents describe how each part works; this one says what the whole must do.

## 1. Vision

codectx is the ultimate context engine for codebases, built for AI agents. On any checkout, from a small project to
the largest enterprise monolith, with zero setup, it lets an agent search precisely and fast, understand code deeply
and quickly, and build sound plans.

The same data lets agents find bugs and vulnerabilities: codectx supplies it, and a review mode helps surface them,
for example during code review. A review harness (a multi-model vulnerability pipeline) can build on codectx and take
its whole front end from it (discovering files, parsing, the call graph, the project profile, analysis units and
reachability, with the algorithms behind them), with better results and richer data. codectx is not the harness:
model tiers, verdicts, findings and reports stay in the harness.

## 2. Users and jobs

| User | Job | Example question |
|---|---|---|
| AI agents over MCP and developers over the CLI; both first-class, same answers | Find code fast and precisely | "Where is `RetryPolicy` defined, and every place that constructs it?" |
| | Understand deeply | "What calls `Charge`, through which interfaces, and what data reaches its `amount`?" |
| | Plan a change | "What breaks if I change this signature? Give me everything I need to read first." |
| | Find bugs and vulnerabilities | "Can a request parameter reach this SQL call without being sanitized?" |
| Review harnesses, over MCP or the CLI | Take all of their code knowledge from codectx | "Give me every analysis unit an entry point can reach, each with its callees, callers and where each parameter's value comes from." |

## 3. Requirements that hold for everything

1. **Every size.** Enterprise codebases and super-large monoliths and monorepos, down to small projects, tuned to no
   single repository.
2. **As fast as possible, with the least memory, never at the cost of data quality, accuracy or results,** at every
   stage and every size: the first index, every refresh after an edit, every query, and peak memory. Disk is free.
3. **Zero setup, any checkout.** The user configures nothing, runs no build and prepares nothing elsewhere.
4. **Complete by default.** No default limit cuts, skips or truncates data; one call returns the complete answer,
   with nothing for the agent to tune. Ranked search returns its best results.
5. **Precise, with its precision stated.** Exact where the language's rules decide it (overloads, generics, inferred
   types, lookup order), matching what the compiler would resolve; a candidate is labelled as one, with the reason;
   every fact carries its precision and its evidence.
6. **Never wrong.** No false edge and no false node: an unproven answer is an open site with a reason, never a guess
   presented as fact.
7. **Never freezes the machine,** even with an index running beside the user's work and many agents at once.
8. **Never regress** output, quality, speed or memory.
9. **All nine languages at the same depth:** C, C++, Go, Java, JavaScript, TypeScript, TSX, Python and Rust,
   including their standard libraries and dependencies and every syntax form of their newest versions. Linux, macOS
   and Windows.

## 4. Capabilities

### 4.1 Find
Ranked code search, symbol lookup, definitions, references, source reading of any declaration or range, repository
overview.

### 4.2 Understand
- **Calls:** callers and callees at any depth, each with its call sites, ranges, line and column; the overrides a
  virtual call can reach; a target per use where the value reaching it is known; why a call is unresolved; every
  same-name candidate labelled "may call", uncapped.
- **Every call is a call site:** explicit type arguments, Rust turbofish, computed callees, constructions,
  destructors, operators, initializer lists, calls inside macros, JSX/TSX elements, decorators, accessors and
  properties.
- **Types:** callable signatures as structure (parameters, types, variadic, return), type parameters and arguments,
  declared and recovered types, element, union, optional and alias types, structural typing (TypeScript object
  literals, Python Protocols, Go interfaces), overload choice and lookup order, re-exports.
- **Metaprogramming:** C/C++ macros, Rust macros, derives and attribute macros, TypeScript and Python decorators,
  Java generated members and annotations, Go `//go:generate`; reflection with constant names.
- **Hierarchy and imports:** extends, implements, overrides; imports bound per name and resolved to files.
- **Data flow:** inside functions and across calls (argument to parameter, return to caller, across every hop), into
  fields, elements and captured variables; control dependence; reads and writes.
- **Into libraries:** calls, types and data flow followed into the standard library and the project's dependencies;
  an external call is one node keyed by package, module or header plus name in every language, so the callees of
  `main` list the standard library everywhere.
- **Change impact and paths** between any two pieces of code.
- **Entry points and reachability:** structural entry files, program mains and scripts, framework routes and their
  handlers, request-input reads, a library's exported API (including Python and C export rules), and everything
  reachable from them over calls and imports.
- **Frameworks and project profile:** languages and frameworks detected from every manifest kind; structural marker
  files; where and how often request input is read; security documentation.

### 4.3 Plan, design and build
During AI development, codectx gives agents the codebase knowledge, context and flows they need to create accurate
plans and designs and to search and find things fast, accurately, and with as few tokens as possible:
- **Token-efficient answers:** the exact code and facts a question needs, in one answer, nothing redundant.
- **Context plans:** for a task, the exact set of code to read, in order, sized to the model's window.
- **Who was given what:** the owner process records every request and what it returned, so a lead agent or a
  reviewer can see which code each agent received, with no extra steps for the agent.

### 4.4 Review: data and a mode for finding bugs and vulnerabilities
For a whole codebase, a diff or a pull request:
- **Analysis units:** each function and method, each file's top-level code (nested bodies replaced by their
  signatures), and each whole file, with exact byte ranges and lines, each readable in one call.
- **Complete review context per unit, in one answer:** callee bodies and caller windows at any depth; the calls into
  a file from other files, with their argument text and ranges; include and import neighbours.
- **Parameter provenance:** for every parameter, where its value comes from, back through every caller hop: a
  constant, a caller's parameter, a local assignment, or the public API boundary when nothing calls it.
- **Reachability per unit:** whether and how an entry point reaches it.
- **Taint flows:** paths from untrusted input (request data, files, environment, deserialized data and the rest) to
  dangerous operations (SQL, shell, file paths, deserialization, templates, redirects and the rest), through library
  calls by models of what each library function does with data, with sanitizers and barriers, each with its evidence
  path.
- **For a change:** what it touches, reaches and breaks, and which flows and entry-point paths it adds or removes.

### 4.5 Keep current
Index, refresh on change, status and doctor; a parse error never damages clean code, and partial results are never
silent (the file, the error and its range are named in every affected answer).

## 5. A review harness's front end, done better

What a harness's front end does today, and what codectx gives instead:

| A harness's front end | codectx |
|---|---|
| Discovers files by extension and size cap | Every source file of the nine languages, no cap; other files kept and readable |
| Call graph that links every same-name definition, capped at 25 | Exact targets, plus every same-name candidate labelled "may call"; no cap |
| Analysis units: each function, a file's top-level code, the whole file | The same units with exact byte ranges, each readable in one call |
| Callee bodies (depth 3) and caller windows inlined into each unit | Callees and callers at any depth, with source and call sites, in one answer |
| Parameter provenance: argument text traced back through callers, up to 4 hops | Data flow from argument to parameter across every hop |
| Cross-file review context | The same, complete, in one request |
| Entry points from the first line of text; reachability as an optional filter | Entry points from code facts; reachability for every unit |
| Framework detection and the facts behind a project trust profile | The same, as structured data |
| Model tiers, verdicts, findings and reports | Stay in the harness; codectx supplies every input they read |

## 6. Interfaces

The MCP server and the CLI are both first-class and give the same answers. One owner process per workspace serves
many agents at once.

## 7. Decided

- PHP is out of scope for now (Oct 9, 2026).
- codectx is the engine a review harness builds on, not the harness (Oct 14, 2026).

## 8. Open decisions

- Whether codectx ever runs a project's build, tests or install scripts, downloads its dependencies, or provisions
  toolchains itself.
- Whether the SCIP indexers and the language servers leave the product, with codectx's own engine as the only source
  of facts.
- Editing code, and a graphical interface.

## 9. How we know it works

- **Accuracy:** answer keys per language, from the compilers' own results and the published test cases of an
  established static-analysis suite: no exact answer wrong, every candidate labelled.
- **A harness takes all of its code knowledge from codectx:** a review harness's front end is replaced by codectx
  answers, with units, context, provenance and reachability at least as complete and more precise.
- **Speed and memory:** every stage profiled and measured on public repositories, small to very large.
- **Host safety:** a full index of the largest of them runs beside normal work with no stall.
- **Release:** the full end-to-end certification on those repositories passes.
