# Native-engine research, raw evidence — "AI does it in 10 minutes": what that gets right and what it gets wrong

The user's framing for this research was: *look at the engine's source and see how easy it would be
to take their logic and put it into Go (or C, C++, Rust) in a fast design.* The honest answer has two
halves and the report must carry both.

## What it gets RIGHT (these really are mechanical)

1. **The algorithms are textbook and the reference is open.** CFG construction, Lengauer-Tarjan
   dominators, the dominance frontier, control dependence (Ferrante §3.1.1) and a forward MOP
   reaching-definitions worklist are all fifty-year-old published algorithms with correctness
   proofs. The engine implements them once, generically, and the code is Apache-2.0 and readable.
2. **The generic core is genuinely small.** Three independent implementations converged on the same
   shape and the same order of magnitude (engine, Semgrep, Fraunhofer — `docs/research/05` §3).
   Nothing here is a research problem.
3. **The product already owns the hard input.** Nine pinned tree-sitter grammars, a per-language
   query pack, a per-language `grammar` hook and function bodies captured as `@body` are shipped
   today (`internal/provider/treesitter/lang/lang.go`, `worker/grammars.go`, `worker/extract.go`).
   A native pass lands in an existing extension point, not a new subsystem.
4. **The facts are already file-local.** The engine itself, run on one file, reproduces 100% of CDG
   and REACHING_DEF for Python, TypeScript, C and Java (`11-incremental-joern.md` §4.3). The
   whole-program JVM run is not what makes those facts correct.

## What it gets WRONG (these are not mechanical and they dominate the schedule)

1. **Per-language lowering semantics.** The generic pass dispatches on a *normalised node
   vocabulary*; producing that vocabulary from nine concrete grammars is the work, and it is where
   every reference implementation is measurably wrong (`docs/research/05` §1.3: Go `defer`/`go`/
   `select`/`fallthrough`/labeled statements unmodelled, JS `throw` not a THROW, Python `yield` a
   `???`). Being *more* correct than the reference is itself a diff to defend.
2. **The differential test corpus.** Sized at 2,500–4,500 lines for CFG+CDG alone
   (`docs/research/05` §4) and it scales with the number of languages parity must be proven for.
   It is plausibly larger than the implementation, and it cannot assert equality: two engine runs
   over the same unmodified tree differ by ~0.01% (`10-round3` §9b), and a claim of equality
   between two engine runs anywhere in this repository is a defect (`providers-dependence.md`).
3. **The precision claim.** The engine stamps `static_analysis`. A tree-sitter pass has an exact
   CFG half and a name-resolved half; the honest label is `syntax` (`docs/research/05` §0). That is
   a **product** decision about what the product promises, not an engineering one, and it cannot be
   made by writing code faster.
4. **`reads`/`writes` has no published algorithm.** It is the engine`s operator target algebra,
   reverse-engineered across six frontends with four documented gaps (`10-round3` §3). There is no
   textbook to copy and no other tool to copy from — no SCIP indexer sets a write role.

## The one-line version

The *algorithm* is a weekend. The *vocabulary mapping for nine grammars*, the *oracle that proves
it matches*, and the *decision to publish a lower precision label* are the project. An estimate
that counts only the first is not wrong about the first; it is silent about the other three.
