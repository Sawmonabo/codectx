# Stable identity and incremental linking: how mature indexers keep a symbol's identity across edits and redo only what an edit affects

Research note for the codectx product owner, behind [ADR-0014](../adr/ADR-0014-stable-identity-incremental-linking.md).
Research date 2026-10-09. The raw excerpts are in [raw/stable-identity/sources.md](raw/stable-identity/sources.md),
cited here as [n].

## 1. The question

Two properties of the code as it stands decide what an edit costs.

- **A located declaration's identity includes its byte range.** `reconcile.CanonicalKey` keys a declaration on its
  file, name, kind, language, qualified name and exact start and end bytes. An edit that changes a function's length
  gives that function, and every later declaration in its file, a new node, so every relation another file publishes
  toward them must be republished.
- **The linker's delta is one digest per file table.** The digest covers the table's declarations, call sites and
  values (no ranges), and the stored resolution names sites and targets by member ordinal and table index. Any
  change to a table, such as a body edit that adds a call, resolves and republishes the whole unit.

For a file `helpers.go` whose `Parse` is called from 5,000 files, the first property republishes 5,000 files on a body
edit that changes no table, and the second relinks the whole project on a body edit that adds one call. The question
is how mature indexers avoid both, completely.

## 2. Identity is a name path, never a position

- **Symbol strings.** The cross-tool index format defines a symbol as a fully qualified name with "one descriptor for
  every node in the AST (along the ancestry path) between the root of the file and the node", and keeps ranges on
  occurrences, separate from the symbol [1]. Its locals are document-scoped [1]. The C/C++ language server keys its
  index on a hash of the unified symbol resolution string [3]. The C# compiler platform builds documentation IDs from
  the qualified name plus each parameter's type [5]. Google's graph schema leaves the signature opaque but requires it
  to distinguish nodes within a corpus [6].
- **The move away from positional IDs.** The Rust language server's AST IDs were positional: adding or removing an
  item invalidated the ID of everything below it in the file. Its June 2025 change replaced numbering with "hash
  important bits, like the name of the item", for about 41 MB more memory on its benchmark (2392 MB to 2433 MB) [8].
- **Overloads.** Typed languages tell overloads apart by parameter types, not order: the C# documentation ID writes
  each parameter's type [5]; the C family's USRs include parameter types for overloads and the return type where
  templates overload by it [4]. The JVM semantic format uses declaration order (`+1`, `+2`) [2], which renumbers an
  overload when another is inserted before it. Parameter types do not.
- **Rebinding languages.** In Python a definition "binds the function name in the current local namespace" [9], so a
  second `def f` at the same level replaces the first, and a name defined in both arms of an `if` is two definitions
  of one name. The enclosing branch distinguishes the second case without any number. For exact duplicates at one
  level, tree differencing matches old to new by a hash of identical subtrees first [10]; order is left only for
  duplicates that both changed and moved.

## 3. An edit redoes only what read what changed

- **The principle.** A build system is minimal if it runs a task "only if they transitively depend on inputs that
  changed" (Definition 2.1), and has early cutoff if it stops when a rebuilt result is unchanged (Fig. 3: a comment
  added to `main.c` stops at an unchanged `main.o`) [11]. A verifying trace records the hashes of what each task read
  and reruns the task only when one of them changed [11]. The incremental query engine behind the Rust compiler and
  language server stops at the first downstream result that did not change [12].
- **Body edits do not reach other files.** The Rust language server's invariant is "typing inside a function's body
  never invalidates global derived data"; its per-file item summary is "stable over modifications to function bodies"
  [7]. The JetBrains platform's per-file stubs hold only what other files need and "depend only on the contents of the
  file" [13]. The JVM build tool skips recompiling dependents for "any change to the body of a method", by hashing the
  ABI [14]. The Go language server keys analysis on hashes of "the types and facts of direct dependencies", so a print
  added to a body "will usually not invalidate most of the graph" [15]; Go's export data is unchanged by a change that
  does not touch exported types [16].
- **Unchanged files are reused.** GitHub's name-resolution graphs index each file in isolation and join files at query
  time [17]; Meta's code index owns facts by unit and hides only changed units [18]; the code-scanning engine overlays
  only the changed files on a cached base [19].

**Inferred results are part of the interface.** Where resolution infers a receiver's type by following values through
assignments, parameters and returns, a body edit can change another file's binding: `def make(): return Dog()` to
`return Cat()` changes what `make().sound()` reaches elsewhere. The interface a file exposes is therefore everything
another file's resolution reads from it, inferred return and parameter values and field writes included, and the
cutoff compares that. This is the Go language server's "types and facts" [15] and the query engine's cutoff on a
derived result [12], applied to inferred summaries.

## 4. A manifest change redoes only what read the changed fields

- The TypeScript compiler marks each option by what it affects: `affectsSourceFile`, `affectsModuleResolution`
  (`baseUrl`, `paths`, `rootDirs`, `moduleResolution`, `module`, `typeRoots`, package `exports`/`imports` resolution,
  `resolveJsonModule`), `affectsSemanticDiagnostics` [20]. `strict` is not a module-resolution option.
- The Go language server once invalidated "all workspace metadata if there is a change to the user's `go.mod`"
  (January 2020) and moved to invalidating only on save and reloading only what is invalid; the report was closed as
  "Obsolete. This doesn't happen any more." (August 2026) [21].
- Applied here: a fingerprint of the fields of each manifest the linker reads (a Go module's path and its nested module
  roots; a TypeScript project's module-resolution options; a package's `exports` and `imports`; a crate's name; a
  Python or Java project root). A dependency version bump changes none of them.

## 5. What this note finds for codectx

1. Key a declaration on file, kind, language, descriptor path and a disambiguator; keep ranges as attributes and
   evidence. Disambiguate by parameter types where the language overloads, by the enclosing branch where a name is
   defined conditionally, and by content-hash matching for exact duplicates. Keep locals and lambdas file-scoped.
2. Split each file's link table into its interface (what other files read, inferred summaries included) and its own
   sites, each with its own digest; key the stored resolution by stable identity.
3. Record a verifying trace per site and reverse indexes from each read key to its sites; on an edit, re-resolve the
   edited file's sites, diff its interface, rerun only the sites whose traces name a changed key, and stop where a
   rerun yields the same result.
4. Diff manifests by the fields the linker reads, and treat a unit-boundary move as a removal and an addition.
5. Prove it with a differential test: after every kind of edit, the incremental generation equals a full rebuild.

The worked example, as counts from the design, not measurements:

| Edit to `helpers.go` (`Parse` called from 5,000 files) | Republished |
|---|---|
| Body edit, with or without a new call | `helpers.go` |
| Rename `Parse` | `helpers.go` and the files whose sites named `Parse` |
| Overload of `Parse` inserted before the existing one | `helpers.go`; existing overloads keep their identities |
| Dependency version bump in `go.mod` | nothing |
