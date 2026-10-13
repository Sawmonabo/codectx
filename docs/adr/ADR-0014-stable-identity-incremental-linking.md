# ADR-0014: A declaration's identity is its name path, and an edit relinks only what read what changed

## Status

Accepted, 2026-10-09, by the owner's ruling. Not yet implemented. This record amends the identity order of the
implementation plan's Section 9.4 and the delta of [ADR-0012](ADR-0012-native-dependence-engine.md) decision 9. The
survey behind it is [research 23](../research/23-stable-identity-and-incremental-linking.md).

## Context

- **Identity moves with bytes.** `reconcile.CanonicalKey` keys a located declaration on its file, name, kind,
  language, qualified name and exact start and end bytes. An edit that changes a function's length gives it, and every
  later declaration in its file, a new node. The linker therefore republishes every file with a call bound into an
  edited file, which is correct and costs one republication per calling file on every such edit.
- **One digest decides everything.** The linker's delta compares one digest per member table, covering declarations,
  sites and values, and its stored resolution names sites and targets by member ordinal and table index. When any
  digest changes, which a body edit that adds a call or a declaration does, the unit resolves and republishes in full.
- **The requirement.** A refresh does the least work its change requires, for any repository at any scale, and gives
  the same facts as a full rebuild.

Worked example: `helpers.go` declares `Parse`, called from 5,000 files. Today a body edit that changes no table
republishes 5,000 files, and a body edit that adds one call relinks the whole project.

## Decision

### 1. A declaration's identity is file, kind, language, descriptor path and disambiguator

- The descriptor path is the declaration's qualified name along its ancestry: each enclosing module, type and
  function. The byte range is an attribute of the node and of its evidence, never part of its key.
- The disambiguator separates declarations that share a descriptor path, in this order:
  1. **parameter types as written**, where the language overloads (Java, C++, TypeScript overload signatures): `parse(String)`
     and `parse(int)` keep their identities wherever a `parse(Path)` is inserted [S1][S2];
  2. **the enclosing branch** where a name is defined conditionally (an `if`/`else` arm, a preprocessor condition):
     both definitions of a name in two arms are distinct with no number [S3];
  3. **content matching** for exact duplicates at one level (a rebinding `def f` written twice): old and new are paired
     by a hash of their content first [S4], then by order among the duplicates no hash matched.
- A parameter is keyed by its declaration's identity and its position, so another file's argument flow into it
  survives a body edit.
- Locals, anonymous functions and call-site nodes keep file-scoped identities. No relation from another file names
  them.
- Every provider that builds a located declaration's candidate supplies these inputs: the structural provider, its
  dependence pass and the precise index importer.
- `renamed_from` and `moved_from` keep carrying lineage across renames and moves.

**Alternatives.** Keeping the range in the key, and republishing every caller: correct, but every length-changing
edit costs one republication per calling file, and the cost grows with how widely a file is used. Disambiguating
every duplicate by declaration order: one surveyed format does, and an inserted overload renumbers its later siblings
[S5]. A positional numbering of items was replaced by name hashing in a production language server for exactly this
reason, at a measured memory cost under 2% [S6].

### 2. Each file's table has two summaries: its interface and its own sites

- **The interface** is everything another file's resolution reads from this file: its declarations and their
  signatures, supertypes, exports, imports and module identity, each function's inferred return and parameter values
  where resolution follows values, and its field writes where resolution is field-based.
- **The sites** are the file's own call sites and values.
- Each has its own digest. A body edit changes the sites; it changes the interface only when it changes something
  another file reads, such as `def make(): return Dog()` becoming `return Cat()`.

**Alternative.** A syntactic interface (declarations and signatures only), as compilers use [S7][S8]. It misses the
inferred values, so a body edit that changes a returned type would leave another file's binding stale. Inferred values
are part of the interface.

### 3. Each site records what it read, and the stored resolution is keyed by identity

- Every bound or unbound site records a verifying trace: each key its resolution read (a name looked up in a scope,
  an interface entry, an inferred summary, a manifest field) with that key's hash [S9].
- Reverse indexes map each key to the sites that read it. They are sorted runs on disk, never a repository-sized
  structure in the heap.
- The stored resolution names sites and targets by identity, not by member ordinal and table index, so an inserted
  declaration renumbers nothing.

### 4. A refresh reruns only the sites whose reads changed, and stops where a rerun changes nothing

- The edited file's own sites always re-resolve.
- Its interface is recomputed and diffed entry by entry; only sites whose traces name a changed entry rerun.
- A rerun that yields the same binding and the same summary stops there. A changed inferred summary propagates to the
  sites that read it, until nothing changes [S9][S10].
- An added file is a set of interface additions, so the sites whose lookups would now see a new name rerun. A deleted
  file is a set of removals.
- Only files whose facts changed are republished; every other fact is carried over.

Worked example, as counts from the design: a body edit to `helpers.go`, with or without a new call, republishes
`helpers.go`; renaming `Parse` republishes `helpers.go` and the files whose sites named `Parse`; inserting an overload
republishes `helpers.go`, and the existing overloads keep their identities.

### 5. A manifest change reruns only the sites that read the changed fields

- Each manifest has a fingerprint of the fields the linker reads: a Go module's path and its nested module roots; a
  TypeScript or JavaScript config's module-resolution and ownership options after `extends` (`baseUrl`, `paths`,
  `rootDirs`, `rootDir`, `outDir`, `declarationDir`, `moduleResolution`, `module`, `customConditions`,
  `resolvePackageJsonExports`, `resolvePackageJsonImports`, `allowJs`, `files`, `include`, `exclude`,
  `references`) [S11]; a package's `name`, `type`, `exports`, `imports`, `types`/`typings`, `module`, `main` and
  `workspaces`, and a pnpm or Lerna workspace's globs; a crate's name; a Python or Java project root.
- An unchanged fingerprint reruns nothing. A Go unit also reads where each package outside it is read from (each
  requirement's version, each replacement, the governing `go.work`, the vendor manifest, the standard library): a
  change to those alone reads those packages again and keeps everything when what they declare is what the previous
  build read (early cutoff [S9]), so a dependency version bump that changes no declaration the unit reads reruns
  nothing.
- A changed field reruns only the sites whose traces read it [S12]. For ECMAScript the read is the specifier's
  answer: each import's resolution is an index entry keyed by the importing file and the specifier, so a changed
  config reruns exactly the nodes whose imports now resolve differently, in whichever package they are written.
- A unit-boundary move, such as a new nested `go.mod`, is a removal from one unit and an addition to the other: the
  moved files' sites, the sites bound into them, and the sites whose lookups see the moved names rerun.

### 6. Proof

The gate is a differential test, for every language: after each scripted edit — body only; body adding a call; new
declaration; rename; overload inserted before an existing one; conditional redefinition; changed returned value in a
language whose resolution follows values; moved field write; file added, deleted and renamed; dependency version
bump; module path change; module-resolution option change; new nested module — the incremental generation equals a
full rebuild, fact for fact. Two further tests fix the cost: a body edit republishes only the edited file, and an
inserted overload keeps every existing overload's identity.

## Consequences

- Body edits cost one file, whatever the file's fan-in.
- Every declaration's and parameter's identity is stable across edits, so search documents, context sessions and
  graph answers keep valid node IDs while code is edited.
- The traces and reverse indexes take disk space proportional to the sites and the keys each reads; their size is
  measured in the scale proofs.
- The structural provider must supply the descriptor path and the disambiguator for every declaration it emits.
- The republication of every calling file on a length-changing edit (Context) is removed once identity no longer
  moves.

## Sources

- [S1] *DocumentationCommentId.cs*, https://source.dot.net/Microsoft.CodeAnalysis/DocumentationCommentId.cs.html —
  parameter types, by-reference marks, generic arity and return type in a member's ID.
- [S2] *USRGeneration.cpp history*, https://src.rcs.uwaterloo.ca/source/history/llvm-project/clang/lib/Index/USRGeneration.cpp
  — parameter types for overloaded functions; return types where templates overload by them.
- [S3] *The Python Language Reference*, §8.7 Function definitions,
  https://docs.python.org/3/reference/compound_stmts.html#function-definitions — a definition binds its name in the
  current namespace.
- [S4] *A Novel Refactoring and Semantic Aware Abstract Syntax Tree Differencing Tool and a Benchmark for Evaluating
  the Accuracy of Diff Tools*, https://arxiv.org/pdf/2403.05939, and Falleri et al., *Fine-grained and accurate source
  code differencing*, ASE 2014 — matching identical subtrees by hash.
- [S5] *SemanticDB specification*, https://scalameta.org/docs/semanticdb/specification.html — order-based overload
  disambiguators; document-scoped locals.
- [S6] *Provide better incrementality when items are changed*, https://github.com/rust-lang/rust-analyzer/pull/19837
  — positional item IDs replaced by name hashes; memory 2392 MB to 2433 MB.
- [S7] *Compilation Avoidance*, https://blog.gradle.org/compilation-avoidance — method-body changes do not recompile
  dependents; an ABI hash decides.
- [S8] *Stub Indexes*, https://plugins.jetbrains.com/docs/intellij/stub-indexes.html — per-file summaries holding only
  what other files need.
- [S9] Mokhov, Mitchell, Peyton Jones, *Build Systems à la Carte*,
  https://www.microsoft.com/en-us/research/uploads/prod/2018/03/build-systems-a-la-carte.pdf — minimality
  (Definition 2.1), early cutoff (Fig. 3), verifying traces (§4.2.2).
- [S10] *How Salsa works*, https://rustc-dev-guide.rust-lang.org/queries/salsa.html — stopping at the first unchanged
  derived result; and *a new analysis driver*,
  https://go.googlesource.com/tools/+/61e2d3f4fee1a8fb382d357fadc53af7828054c0%5E%21 — analysis keyed on the types and
  facts of direct dependencies.
- [S11] TypeScript v5.9.2 `src/compiler/types.ts` (option flags) and `commandLineParser.ts`,
  https://github.com/microsoft/TypeScript/tree/v5.9.2/src/compiler — options marked by what they affect.
- [S12] *diff metadata before invalidating on changes to go.mod*, https://github.com/golang/go/issues/36589 — from
  invalidating all metadata on a manifest change to reloading only what is invalid.
- [research 23](../research/23-stable-identity-and-incremental-linking.md) and
  [raw/stable-identity](../research/raw/stable-identity/sources.md) — the full survey and the quoted excerpts.
