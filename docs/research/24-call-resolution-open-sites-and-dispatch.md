# Calls no analysis can bind, and calls through a base class: what mature tools answer and what codectx should

Research note for the codectx product owner, behind [ADR-0015](../adr/ADR-0015-open-call-sites-and-dispatch.md).
Research and hands-on tests on 2026-10-09. The full write-ups, every fixture, script and raw language-server
transcript are in [raw/call-resolution/](raw/call-resolution/):
[name-only-calls.md](raw/call-resolution/name-only-calls.md) (calls bound by name alone) and
[dispatch-calls.md](raw/call-resolution/dispatch-calls.md) (calls through a base class), with the probe tree under
`raw/call-resolution/probe/`. Scanner databases (about 104 MB) are left out; their query results are kept.

## 1. Calls no analysis can bind

**The question.** When the linker cannot justify a binding by a scope, an import, a type or a signature
(`obj.handle()` with no recovered type for `obj`), what should "who calls `handle`?" return?

**What the field does** (name-only-calls.md §2, §3):
- Language servers (gopls, pyright, tsserver, rust-analyzer) leave such calls out and say nothing. rust-analyzer's
  find-usages starts from a text search and keeps only what name resolution confirms.
- Code-graph tools built for agents (code-review-graph, code-graph-rag, codebase-memory-mcp, trace-mcp) show name
  matches by default but tag each with its basis; code-review-graph counts the name-only sites its impact walk will
  not follow, "an upper bound on missed impact".
- Measured cost of guessing: PyCG reaches 99.2% precision but 69.9% recall on five Python projects; scored by call
  chains, one wrong edge poisons every later step (Jarvis: PyCG drops to 0.19 precision). Field-based JavaScript
  resolution, close to name matching for member calls, keeps 65–84% per-site precision (ACG).
- codectx's own measurement: 70.59% of call sites share a callee name with some tracked definition, so folding name
  matches into `calls` would make most new edges wrong.
- Hub names: one tool measured `list_head` at 7,188 definitions on the Linux kernel; stored name edges multiply by
  that, and capping them hides real calls.

**Finding.** Keep `calls` for justified bindings. Publish each unbound site once, with its reason, and offer
same-name declarations as a separate, labelled relation worked out at query time: ranked, counted exactly, paged,
never walked transitively by default, and never a bare "0 callers" while one exists.

## 2. Calls through a base class

**The question.** With `speak(a: Animal) { a.sound() }` and `Dog`, `Cat` overriding `sound`, what should "who calls
`Dog.sound`?" return, and how should the answer show that `speak` reaches it only through `Animal.sound`?

**Hands-on results** (dispatch-calls.md §2, six language servers, raw JSON-RPC in `probe/out/`):

| Server | Callers of `Dog.sound` include `speak`? | Marked? | Adds `direct(d: Dog)` to `Animal.sound`'s callers? | Three levels right? |
|---|---|---|---|---|
| tsserver / TypeScript 7 | yes | no | yes (also under `Cat.sound`) | no |
| jdtls | yes | no | no | yes |
| clangd | yes | computes a flag, never sends it | no | no (one level; lists a qualified call that cannot dispatch) |
| gopls | yes | no | yes | — |
| pyright | no | — | no | no |
| rust-analyzer | no | — | yes | — |

Every server's outgoing calls from `speak` stop at `Animal.sound`.

**Stored form in other tools** (dispatch-calls.md §3): code-navigation indexes (CodeQL Java's static callee, Kythe's
`ref/call`, SCIP's reference to `Animal#sound()`, code-graph-rag's `CALLS` plus `OVERRIDES`) store one edge to the
declared method and compute the dispatch view (CodeQL `polyCalls`, Kythe's override walk). Whole-program analyzers
(Soot, SootUp, WALA, OPAL, PyCG, Jarvis) store one edge per possible target; with an abstract base, "who calls
`Animal.sound`" is empty in all four Java tools, and with no object flowing in, the flow-based ones report no caller
at all. No tool uses a separate relation kind for dispatch calls. Eclipse JDT core flags a call on a supertype
receiver (`isSuperInvocation`, "may or may not be the search target") and leaves out calls on a subtype that
overrides the method (bug 160301); jdtls drops the flag.

**Findings.**
1. Store one `calls` edge per site to the method the receiver's declared type names, plus `overrides` to the method
   directly overridden; compute deeper levels at query time.
2. "Who calls `m`" adds the callers of every method `m` overrides, at any depth, but only calls that can dispatch
   (not `super`, qualified, constructed-object, final, static or non-virtual calls), each marked with the base
   method, counted apart. Never add an override's direct callers to a base's callers.
3. "What does `speak` call", paths and forward impact need the same step downward.
4. The mark must reach the answer: two tools compute it and drop it.
5. Whether a call can dispatch must be recorded as its own field: the linker's technique tags carry it only for the
   static languages (a Python `super()` call and flow-bound calls carry the same tag whether they dispatch or not).
6. With no declared type and several classes flowing in, publish one `calls` per class's method, marked "1 of n", as
   the flow-based analyzers do (without the mark).

**Not tested hands-on** (name-only-calls.md §6, dispatch-calls.md §3 and §8): a Hack indexer newer than the
2023 build that was run; code-graph-rag's libclang C++ and jedi Python modes; Kythe's override-callers view, which
its open-source table builder never fills; a live Sourcegraph instance; OPAL's incomplete-call-site report; and the
cause of one empty direct Glean `DeclarationUses` query. Their rows rest on documentation and source.

## 3. Sources

The complete source lists, with commits and line numbers, are §6 of name-only-calls.md and §7 of dispatch-calls.md.
The ones the findings rest on:
- *Navigating the call graph*, CodeQL, https://codeql.github.com/docs/codeql-language-guides/navigating-the-call-graph
- *Call graph*, Kythe, https://kythe.io/docs/schema/callgraph.html
- *scip.proto*, https://github.com/sourcegraph/scip/blob/main/scip.proto
- *[clangd] Show callers of base functions in incomingCalls*, https://llvm.googlesource.com/llvm-project/+/378e4ed1acf94ee911e2ccfda0045ac3fdc9e291
- *jdt-ui-dev thread on call hierarchy and overriding methods*, https://www.eclipse.org/lists/jdt-ui-dev/msg01526.html
- *SootUp call graphs*, https://soot-oss.github.io/SootUp/latest/callgraphs/
- *callgraph/cha*, Go, https://pkg.go.dev/golang.org/x/tools/go/callgraph/cha
- Salis et al., *PyCG: Practical Call Graph Generation in Python*, ICSE 2021; Huang et al., *Jarvis*, arXiv
  2305.05949; Feldthaus et al., *Efficient Construction of Approximate Call Graphs for JavaScript IDE Services*, ICSE
  2013; Laursen, Xu, Møller, *Reducing Static Analysis Unsoundness with Approximate Interpretation*, PLDI 2024.
