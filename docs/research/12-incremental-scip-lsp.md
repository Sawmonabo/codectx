# Incremental re-indexing: what SCIP indexers and LSP servers can actually skip

Round 4 empirical research, 2026-09-13, linux/amd64 (WSL2, 48 GB RAM, 16 threads).

**Question.** After the first full index, when a user edits one file or commits a few
files, can any of our six SCIP indexers do less work than a whole-unit re-index? What does
the SCIP format itself allow for document-level replacement?

**Short answer.** Only one of the six (scip-java) has a real per-file incremental
pipeline. Two more (scip-go, scip-clang) accept a narrowed input and produce documents that
are exactly equal to the whole-unit ones, with one named exception each. The other three
(scip-typescript, scip-python, rust-analyzer) must re-analyse the whole unit, and
narrowing either changes symbol names or saves nothing. **But the finding that matters most
for codectx is different and it is measured: after a whole-unit re-run, the number of
*documents* that actually change for a one-file edit is 1 — out of 141, 954, 76, 1316 and
2 documents on the five corpora measured.** So the importer can replace one document and
reuse the rest, which is where almost all of the storage, FTS and reconcile cost lives.

Every claim below is marked **SOURCED** (primary document or source code, URL given),
**MEASURED** (run on this machine, command and numbers given), or **INFERRED**.

---

## 1. What the SCIP format allows

### 1.1 Documents are independent; the wire format has no recency marker

The canonical repository moved: `github.com/sourcegraph/scip` now 301-redirects to
`github.com/scip-code/scip`, and the Go module path is
`github.com/scip-code/scip/bindings/go/scip`. **MEASURED** — `go get
github.com/sourcegraph/scip/bindings/go/scip@latest` fails with `module declares its path
as: github.com/scip-code/scip/bindings/go/scip`. **SOURCED** —
<https://github.com/scip-code/scip>, rename commit `d2ceaffef73a` (2026-03-24).

From `scip.proto` (**SOURCED**,
<https://raw.githubusercontent.com/scip-code/scip/v0.10.0/scip.proto>):

| Spec text (verbatim) | Consequence for us |
|---|---|
| "An index contains one or more pieces of information about a given piece of source code… Complementary information can be **merged together from multiple sources**" | Merging is a sanctioned use |
| "To permit streaming consumption… the `metadata` field must appear at the start of the stream and must only appear once… **Other field values may appear in any order**" | `repeated Document` is order-free; documents are not positionally coupled |
| `relative_path` — "(Required) **Unique** path to the text document." | Path is the natural replacement key |
| `Document.text` — "(optional)… Indexers are **not expected to include the text by default**" | **MEASURED**: all six indexers emitted `docs_with_text=0`. A document carries no version, no timestamp and no content hash |
| `local <local-id>` — "Local symbols **MUST only be used for entities which are local to a Document**, and cannot be accessed from outside the Document" | Local symbol ids are document-scoped, so replacing a whole document is the correct unit |
| `position_encoding` is a **Document** field | A replaced document is self-describing even if it came from a different indexer |
| `external_symbols` is an **Index** field | Not replaceable per-document; must be reconciled separately |

**The format's gap:** because a `Document` has no version, timestamp or hash, when two
documents share a path *the wire format cannot say which is newer*. Replacement is
therefore something a consumer does, not something the format does.

### 1.2 An index may legally hold two documents with the same path — and consumers disagree about it

`scip.proto` says nothing about duplicate paths (**SOURCED**, absent at `main`, at tag
`v0.10.0`, and at the first commit `3a3ca0c636d1`). The behaviour is defined only in code,
and the two reference consumers in the same repository resolve it **differently**:

- `bindings/go/scip/flatten.go`: "FlattenDocuments **merges** elements of the given slice
  with the same relative path… `existing.Symbols = append(existing.Symbols,
  document.Symbols...)`" → **union**. **SOURCED**, and **MEASURED** by reading the module
  in `$GOMODCACHE/github.com/scip-code/scip/bindings/go/scip@v0.10.0/flatten.go`.
- `cmd/scip/convert.go` uses `relative_path TEXT NOT NULL UNIQUE` and logs `found multiple
  documents with identical relative path; ignoring duplicates … firstIndex=0
  duplicateIndex=2` → **first wins**. **SOURCED**.
- `scip lint` reports duplicates as a **warning**, not an error: `warning: found multiple
  documents with path '%s' in index`. **SOURCED**.

**Consequence for the importer:** concatenating a fresh document onto an existing index is
*not* replacement. One consumer would keep the stale occurrences alongside the fresh ones
(sorted by range, with no recency signal); another would keep the stale document and drop
the new one. codectx must do an explicit keyed delete-then-insert. **INFERRED from the two
sourced behaviours.**

**MEASURED in all six indexers' own output:** none of them ever emitted two documents with
the same path (`duplicate_paths=0` on every index produced in Section 3). Duplicates are a
merge hazard we would create, not something the tools hand us.

### 1.3 Symbol strings are stable enough to join across documents — with named exceptions

The spec states only that a symbol "should serve as a unique identifier **across the
package**". There is **no spec guarantee of stability across runs or across indexers**
(**SOURCED**: grep of `scip.proto`, `docs/scip.md`, `DESIGN.md`, `README.md` for
`stable|determinis|across` found nothing). Structurally it cannot hold across indexers,
because `<scheme>` is the first token of every global symbol and `<version>` is embedded in
`<package>`.

We measured stability across runs and across scopes instead (Section 3.2 and 3.4), and
found two real exceptions, both of which the plan must handle.

### 1.4 Why this was not possible in LSIF

**SOURCED**, <https://sourcegraph.com/blog/announcing-scip>, verbatim:

> Complexity of implementing incremental indexing… The heavy usage of opaque global IDs
> imposes an ordering constraint on how symbols (or 'resultSet') get added to the index…
> **Globally incrementing IDs make it difficult, as well, to update an existing index with
> new information for only a subset of the documents.**

And on SCIP itself, note the modality:

> Going forward, we anticipate SCIP additionally unblocks the following use-cases…
> **Incremental indexing**: **once implemented**, SCIP users will experience shorter
> waiting time…

A full clone of `scip-code/scip@main` grepped for `incremental|partial index|partial
update` yields exactly two hits, neither of them a capability claim (**SOURCED**). So:
SCIP makes document-level replacement *possible* where LSIF made it impractical, but SCIP
does not *implement* it, and no upstream tool does.

### 1.5 The `scip` CLI has no merge

**MEASURED**, `scip --version` → `v0.10.0`; `scip --help` lists exactly `lint, print,
snapshot, stats, test, expt-convert, help`. **SOURCED**: no `merge`, `combine`, `diff` or
`union` subcommand has existed in any of the 20 released versions (`cmd/scip/` contains
`convert.go, lint.go, main.go, option_from.go, print.go, snapshot.go, stats.go, test.go`).
The Go library offers `FlattenDocuments` (a union), `CanonicalizeDocument`, `SortDocuments`
and `ParseStreaming` ("Parsing takes place at **Document granularity**"), but **no
`MergeIndexes` and no `ReplaceDocument`**. Index merging is ours to own.

`scip lint`'s most useful check for us is
`missingSymbolForOccurrenceError`: "found occurrence at … for symbol …, but there is no
matching SymbolInformation in external symbols or any document" — a cross-document
integrity check we can use as a splice test (Section 3.5). **SOURCED** + **MEASURED**.

### 1.6 Sourcegraph, the format's author, does not do document-level updates

**SOURCED**. Uploads replace wholesale per `(repository, commit, root, indexer)`, enforced
by a unique database index: `DeleteOverlappingCompletedUploads(ctx, repositoryID, commit,
root, indexer)` with the comment "This is necessary to perform during conversions… as
there is a **unique index on these four columns**". The only partition axis is `-root`
(sub-project, never file). Their answer to staleness is *tolerance*, not patching:

> With periodic jobs, you should still receive precise code navigation on non-indexed
> commits **on lines that are unchanged since the nearest indexed commit**. This requires
> that the indexed commit be a direct ancestor or descendant **no more than 100 commits**
> away.

Auto-index cadence defaults: scheduler tick 2 minutes,
`PRECISE_CODE_INTEL_AUTO_INDEXING_REPOSITORY_PROCESS_DELAY` = **24 hours per repository**.
Their tree-sitter SCIP emitter (`scip-syntax`, in
`docker-images/syntax-highlighter/crates/scip-syntax/`) *is* per-file — `fn
index_content(contents: &str, parser_id, options) -> Result<Document>`, a pure function
with no cross-file state, and a CLI mode `index files <filenames...>` — but the production
worker still pipes a whole-commit tarball and uploads at `Root: ""`. **The per-file
capability exists in the tool and is unused in the pipeline.** All **SOURCED**.

---

## 2. Per-indexer capability from upstream sources

All **SOURCED** unless marked.

| Indexer | Narrowing mechanism | Own cache | Upstream incremental status |
|---|---|---|---|
| **scip-go** 0.2.7 (`github.com/scip-code/scip-go`) | `index [<package-patterns>...]`, added by PR #139 "Allow passing arbitrary package patterns" | none written; relies on the **Go build cache** via `go list -export=true` and a load mode that omits `NeedDeps` (PRs #214, #222) | issue #80 "Investigate using go/analysis… **enabling incremental indexing**" — **closed `not_planned`**, 2026-02-05 |
| **scip-typescript** 0.4.0 | `index [projects...]`; a project's file set comes from its tsconfig `files`/`include`; recurses into `projectReferences` | in-process only: `--no-global-caches` toggles a `Map` of SourceFiles/ParsedCommandLines shared **between projects in one run** (PR #182); **never persisted** | none; no `.tsbuildinfo` use at all (grep for `tsbuildinfo\|incremental\|createIncrementalProgram` in `src/` → 0 hits). Issue #175 closed `not_planned` |
| **scip-python** 0.6.6 | `--target-only <path>` — "limit analysis to the following path"; implemented as `file.startsWith(resolve(targetOnly))` + `program.setTrackedFiles(...)` | none; the vendored pyright was modified to **remove** its bail-out and memory-eviction heuristics so it analyses everything | none |
| **rust-analyzer** 1.98 `scip` | positional `<path>` accepts a workspace member, but it is a pure **output filter** (`strip_prefix` fails → document silently skipped) | none. "The analyzer keeps all this input data **in memory and never does any IO**" | issue #4712 "Persistent caches" **open since 2020-06-02**; #18140 "SCIP indexing is single threaded" open (mozilla-central: 19m49s at 110 % CPU) |
| **scip-java** 0.13.1 (`github.com/scip-code/scip-java`) | **javac compiler plugin emits one SCIP shard per source file**; `scip-java aggregate --targetroot <dir>` merges a directory of shards. (`index-semanticdb` and SemanticDB were **removed in v0.12.0**; the plugin now writes SCIP directly) | the **targetroot is a durable per-file shard store** | works, undocumented. `scip-java index` deliberately defeats it: Maven path forces `-Dmaven.compiler.useIncrementalCompilation=false` and `clean verify`; Gradle path does `targetroot().deleteRecursively()` then `clean` |
| **scip-clang** 0.4.0 | documented workflow is to **subset `compile_commands.json` with `jq`**; driver/worker model writes per-TU shards then merges | `--temporary-output-dir` keeps shards keyed by **positional job id**, never read back | issue #183 "Incremental builds support" **closed NOT_PLANNED**, 2026-01-03, all three sub-tasks unchecked |

Three corrections to assumptions this project has been carrying, all **SOURCED**:

1. scip-java no longer uses SemanticDB, and `sourcegraph.github.io/scip-java` is HTTP 404;
   docs are at `scip-code.github.io/scip-java`. The plan's Section 11.7 upstream column for
   `scip-java` should eventually say `scip-code/scip-java`.
2. scip-go's flag is `--repository-remote`, not `--repository-root`. **MEASURED** from
   `scip-go index --help`.
3. scip-clang's flags are `--temporary-output-dir` and `--print-statistics-path`.

---

## 3. Measured experiments

### 3.0 Setup

Tools (all already installed, versions confirmed by `--version`): `scip` v0.10.0, scip-go
0.2.7, scip-typescript 0.4.0, scip-python 0.6.6, scip-java 0.13.1, scip-clang 0.4.0,
rust-analyzer 1.98.0, Go 1.27.1, Node 22.23.2, Temurin 21.0.12, Maven 3.9.11, Python 3.14
venv with pip 25.1.1 (scip-python refuses to run without pip on `PATH`: `Could not find
valid pip command`).

Two helper programs were written for this round (Go, using the official
`github.com/scip-code/scip/bindings/go/scip` bindings), and live in the scratchpad:

- `dochash` — for each document, run `FlattenDocuments` then `CanonicalizeDocument`, then
  `proto.Marshal` with `Deterministic: true`, and print `sha256[:16]`, path, occurrence
  count, symbol count. Also modes `meta`, `dup`, `occ`, `doc`, `parts`, and a `-nopath`
  flag that excludes `relative_path` from the hash. **Comparisons below are of the
  canonicalized document, not of raw protobuf bytes** — raw field ordering is not
  guaranteed stable, so "identical" always means canonically identical.
- `docswap base.scip patch.scip out.scip <path>...` — replaces the named documents in
  `base` with the versions from `patch`. This is a model of the importer's proposed
  document-level replacement.

Corpora:

| Corpus | Language | Files | Lines | Unit |
|---|---|---|---|---|
| `codectx` (this repo) | Go | 128 `.go` | 37,657 | one Go module |
| `golang/tools` @ HEAD | Go | 1,937 `.go` (954 in the root module) | 406,609 | one Go module |
| `microsoft/TypeScript` v5.8.3 `src/compiler` | TypeScript | 77 `.ts` | 190,858 | one tsconfig project |
| `python/mypy` @ HEAD | Python | 1,316 indexed | 129,378 in `mypy/` | one package tree |
| synthetic 4-class Maven module | Java | 4 | ~20 | one Maven module |
| synthetic 4-TU C project + headers | C | 6 | ~20 | one `compile_commands.json` |
| synthetic 2-crate Cargo workspace | Rust | 2 | ~8 | one Cargo workspace |

### 3.1 Whole-unit baseline cost

`/usr/bin/time -f "%e %M"`, warm caches, best of two identical runs.

| Corpus / indexer | Command | Wall | Max RSS | Index bytes | Documents | Occurrences |
|---|---|---|---|---|---|---|
| codectx / scip-go | `scip-go index --output X -q` | **0.29 s** | 118 MB | 6,504,767 | 141 | 72,086 |
| codectx / scip-go, **cold `GOCACHE`** | same, `GOCACHE=<empty dir>` | **10.38 s** | 823 MB | — | — | — |
| golang/tools / scip-go | `scip-go index --output X -q` | **0.68 s** | 391 MB | 22,670,073 | 954 | 291,714 |
| golang/tools / scip-go, **cold `GOCACHE`** | same | **6.97 s** | 369 MB | — | — | — |
| TS src/compiler / scip-typescript | `scip-typescript index --output X --no-progress-bar src/compiler` | **6.27 s** | 1,004 MB | 23,150,871 | 76 | 249,787 |
| mypy / scip-python | `scip-python index --project-name mypy --project-version 1.0 --output X --quiet` | **84.66 s** | 2,947 MB | 55,566,114 | 1,316 | 538,538 |
| jmod / scip-java | `scip-java index --output X` (drives Maven) | **2.07 s** | 268 MB | 3,543 | 4 | 36 |
| jmod / `scip-java aggregate` only | `aggregate --no-parallel --targetroot target/scip-targetroot` | **0.29 s** | 116 MB | 3,543 | 4 | 36 |
| cmod / scip-clang | `scip-clang --compdb-path compile_commands.json --index-output-path X` | **0.17 s** | 29 MB | 2,896 | 5 | 62 |
| rsws / rust-analyzer | `rust-analyzer scip . --output X` | **1.56 s** | 593 MB | 3,588 | 2 | 35 |

**MEASURED.** The Go cold/warm split is the single most important number here: scip-go
itself caches nothing, but it loads dependencies from compiler **export data** (`go list
-export=true`), so it rides the Go build cache. That cache is file-level incremental, which
is why a warm whole-module re-index of a 406k-line module costs 0.68 s and a cold one costs
6.97 s. **INFERRED** consequence: for Go, "re-run the whole unit" is already nearly free as
long as we do not evict `GOCACHE`.

### 3.2 Are two identical runs identical? (Baseline for any change detection)

| Indexer / corpus | Identical runs | Documents with a different canonical hash | Cause |
|---|---|---|---|
| scip-go / codectx | 6 | **2 of 141** (`internal/snapshot/snapshot_test.go`, `internal/provider/treesitter/worker/grammars.go`) | non-unique symbol names, below |
| scip-go / golang/tools | 2 | **3 of 954** | same |
| scip-typescript / TS | 2 | **0 of 76** | — |
| scip-python / mypy | 2 | **0 of 1316** | — |
| rust-analyzer / rsws | 2 | **0 of 2** | — |
| `scip-java aggregate --parallel` | 3 | **0 of 4** documents, but the **file bytes differ every run** | document order varies |
| `scip-java aggregate --no-parallel` | 2 | 0, file bytes identical | — |

**MEASURED.** Two findings:

1. **scip-go emits non-unique global symbol names, and this makes some documents
   nondeterministic.** In `internal/snapshot/snapshot_test.go`, two distinct declarations —
   `var _ Catalog` and `var _ Store` — both receive the symbol
   `` scip-go gomod github.com/Sawmonabo/codectx . `…/internal/snapshot`/_. ``. Canonicalization
   merges them by name and which one survives depends on emission order. In
   `grammars.go` the collisions are function-local: `` …/worker`/grammars:d. `` appears 8
   times, `grammars:_.` 6 times, `grammars:bool.` 5 times. Repo-wide, 3 of 141 codectx
   documents contain at least one repeated symbol name. **This is a direct hit on plan
   §9.4** ("Overloads, shadowed variables, anonymous declarations… must not merge by short
   name"): a SCIP global symbol name is not always a unique native key, so
   `native_aliases` must tolerate a `(scope_key, native_key)` that maps to more than one
   node, and a document-changed test must not treat these two files as "changed".
2. **`scip-java aggregate` is parallel by default and the output file bytes vary**, but
   every *document* is identical. Because codectx imports at document granularity and keys
   by path, `--parallel` is safe for us; only a file-level content hash of the `.scip`
   would be fooled.

### 3.3 Blast radius of a one-file edit, with a whole-unit re-run

This is the question the plan's §8.2 and §13.1 actually need answered. Edit one file,
re-run the whole unit, count how many *documents* changed.

| # | Corpus | Edit | Re-run wall | Documents changed (excluding the known-flaky ones) |
|---|---|---|---|---|
| A | codectx | append an unexported func to `internal/bench/doc.go` (leaf) | 0.32 s | **1 of 141** |
| B | codectx | append an **exported** func to `internal/model/source.go`; `internal/model` is imported by 92 of the 128 Go files | 0.96 s | **1 of 141** |
| C | codectx | rename `model.DecodeID` → `DecodeIdent` across the module (8 files edited) | 0.98 s | **8 of 141** — exactly the 8 edited files |
| D | golang/tools | append an exported func to `go/ast/inspector/inspector.go` (imported by 118 files) | 2.12 s | **1 of 954** |
| E | golang/tools | append an unexported func to `go/analysis/passes/nilness/nilness.go` | 0.78 s | **1 of 954** |
| F | TS src/compiler | append an exported func to `core.ts` (imported everywhere) | 6.48 s | **1 of 76** |
| G | TS src/compiler | append a non-exported func to `watch.ts` | 8.95 s | **1 of 76** |
| H | TS src/compiler | append a function with an **anonymous type literal** to `binder.ts` | 6.27 s | **1 of 76** |
| I | mypy | append a function to `mypy/nodes.py` (imported by most of the package) | 83.39 s | **1 of 1316** |
| J | jmod | change a method body and add a method in `B.java` | 2.08 s | **1 of 4**; and **1 of 4 shard files** changed byte-for-byte |
| K | rsws | add a `pub fn` to `alpha/src/lib.rs` | 1.55 s | **1 of 2** |
| L | codectx | add a method to the `provider.Sink` **interface** that implementers already have (4 files edited, none of them `internal/storage/sqlite/units.go`) | 0.46 s | **5 of 141** — the 4 edited files **plus `internal/storage/sqlite/units.go`, which was not edited** |
| M | codectx | **delete** `internal/provider/provider_test.go` | 0.28 s | 141 → **139** documents; no document is emitted for the deleted path |

**MEASURED.** Three things follow.

- **Adding a declaration does not disturb referencing documents.** Cases B, D, F, I each
  added a symbol to a module everything imports, and the referencing documents were
  unchanged, because their occurrence ranges and symbol strings did not change. The
  intuition that "everyone imports it, so everyone re-indexes" is wrong at document level.
- **Renaming does, and so does changing a type — the changed-document set is not always
  the changed-file set.** Case C changed exactly the 8 documents whose text changed, because
  a Go rename is not a rename in SCIP terms, it is an edit to every file that spells the
  name. But case L is the counterexample, and it is **MEASURED, not inferred**: adding
  `UnitID() model.UnitID` to the `provider.Sink` interface (a method its implementers already
  had, so nothing else needed editing) changed the document for
  `internal/storage/sqlite/units.go`, which nobody touched. Its occurrence count and symbol
  count were identical before and after (2,553 occurrences, 424 symbols); the *only*
  difference in the whole document was one relationship:

  ```text
  - SYM …`/internal/storage/sqlite`/UnitWriter#UnitID(). kind=Method rel=[]
  + SYM …`/internal/storage/sqlite`/UnitWriter#UnitID(). kind=Method rel=[…`/internal/provider`/Sink#UnitID.(impl=true)]
  ```

  This is the mirror image of the scip-go narrowing defect in §3.4: `is_implementation`
  appears and disappears with the visible package set. The consequence for the plan is
  **not** that document replacement is unsound — it is that the importer must compute the
  changed-document set by **comparing canonical document hashes from the fresh index**, and
  must never assume it equals the set of files git reports as changed. Case H (an anonymous
  type literal, which scip-typescript names with a project-global memoized counter) probed
  the same hazard in TypeScript and did *not* spread.
- **A deleted file leaves no trace to clean up, but also no tombstone.** Case M: after
  deleting one source file, the fresh index simply has no `Document` for that path
  (141 → 139 documents — the source file, plus the `go test` main it generated). scip-go
  does not emit an empty document as a deletion marker. So the importer's rule must be
  *set-based, not patch-based*: documents present in the stored unit but absent from the
  fresh index are deleted. A path-keyed "replace what changed" loop alone would silently
  retain facts for files that no longer exist, and plan §9.4 makes a rename a new FileID —
  i.e. every `git mv` hits this path.
- **Editing the file does not make the re-run cheap, but it does make the import cheap.**
  Wall time is dominated by whole-unit analysis in every case (Python: 83 s for a
  one-line edit). What collapses is the *import*: 1 of 1316 documents, ~42 KB of 55 MB.

### 3.4 Narrowed re-index: what each tool will accept, and whether it is exact

#### scip-go — package patterns: exact except for interface implementations

```
scip-go index ./internal/storage/... --output N.scip -q          # codectx
scip-go index ./go/analysis/...       --skip-implementations …   # golang/tools
```

| Run | Wall | Documents | Canonically identical to the whole-module run |
|---|---|---|---|
| codectx, `./internal/model` (leaf package) | 0.15 s | 16 | **16 of 16** |
| codectx, `./internal/storage/...` | 0.19 s | 12 | 6 of 12 |
| codectx, `./internal/storage/...` **with `--skip-implementations` on both sides** | 0.18 s | 12 | **12 of 12** |
| golang/tools, one package (`./go/analysis/passes/nilness`) | 0.12 s | 4 | 3 of 4 |
| golang/tools, `./go/analysis/...` with `--skip-implementations` on both sides | 0.24 s | 280 | **277 of 280** |

**MEASURED.** Two distinct failure modes, both isolated:

1. **`SymbolInformation.relationships` with `is_implementation` is a whole-module
   computation.** In the narrowed codectx run, `UnitWriter#PutNodes()` lost its
   `rel=[…/internal/provider`/Sink#PutNodes.(impl=true)]` edge — 12 relationships across 6
   documents. Occurrences were **identical: 2,553 of 2,553 in `units.go`, 0 differences**.
   Widening the pattern to include the interface's package did *not* fix it (it made it
   worse: 9 of 18 documents differed), because the relation set on a type depends on every
   package indexed. `--skip-implementations` on both sides makes narrowing exact.
2. **Interface-method descriptors change shape when the defining package is outside the
   pattern.** Over the 280-document `./go/analysis/...` narrowing, 45,453 occurrences were
   compared and **11 differed**, all of one kind:

   ```
   whole-module:  …`golang.org/x/tools/go/ssa`/Value#Type.
   narrowed:      …`golang.org/x/tools/go/ssa`/Value#Type().
   ```

   `Instruction#Pos.` vs `Instruction#Pos().`, `CallInstruction#Common.` vs `…Common().`,
   `NamedOrAlias#TypeArgs.` vs `…TypeArgs().` — affecting 3 of 280 documents. Both indexes
   are internally consistent; they simply disagree, so splicing narrowed documents into a
   whole-module index would create dangling references at a rate of about **0.02 % of
   occurrences**. **INFERRED**: a narrowed scip-go refresh is exact only if the narrowing
   pattern covers the dependency closure of the changed packages, or if we accept and
   repair this one descriptor shape.

`scip-go missing [<patterns>]` is *not* an incremental primitive: it reports source files
for which the indexer produced no Document (a coverage self-check). **MEASURED**: on
codectx and on the narrowed pattern it printed `No missing documents`. **SOURCED**:
`ListMissing` in `internal/index/scip.go`.

`ProjectRoot` stays at the module root for a narrowed run and document paths stay
module-relative. **MEASURED** — the narrowed index reported the same `projectRoot` as the
full one. This is what makes scip-go narrowing spliceable at all.

#### scip-typescript — tsconfig `files` narrowing: exact occurrences, unstable anonymous-type names

A separate `tsconfig.json` with an explicit `files` list, extending the project's base
config:

| `files` count | Wall | Documents | Canonically identical to whole-project | Nature of the differences |
|---|---|---|---|---|
| 1 (`watch.ts`) | **1.21 s** | 1 | 0 of 1 | **occurrences 2038/2038 identical, symbols 385/385 identical**; the only JSON difference was two hover strings where a union type printed as `BuilderProgram \| Program` instead of `Program \| BuilderProgram` |
| 5 | 1.59 s | 5 | 1 of 5 | see below |
| 20 | 3.17 s | 19 | 9 of 19 | see below |
| 38 | 4.74 s | 37 | **37 of 37** | — |
| 77 (whole project) | 6.27 s | 76 | — | baseline |

**MEASURED.** Narrowing works and is fast (1.21 s vs 6.27 s for one file), and the file set
is exactly the `files` list — imported-but-unlisted files are *not* emitted as documents.
But at 20 files, 6,428 of 91,558 occurrence lines differed, and the cause is specific:

```
whole project:  …/`commandLineParser.ts`/parseConfigFileTextToJson().typeLiteral1391:config.
narrowed (20):  …/`commandLineParser.ts`/parseConfigFileTextToJson().typeLiteral201:config.
```

`FileIndexer.ts:579` names anonymous type literals `'typeLiteral' +
this.localCounter.next()`. `localCounter` is per file, but the resulting symbol is memoized
in a **project-global** `globalSymbolTable: Map<ts.Node, ScipSymbol>`, so the number is
assigned by whichever file reaches the node *first*. Change the project's file set and the
first-toucher changes. **SOURCED** (the source line) + **MEASURED** (the divergence and its
disappearance at 38 files, where the traversal order happened to coincide). `local N` ids
also shift, but those are document-scoped by spec and therefore harmless under whole-document
replacement.

**INFERRED:** narrowing a tsconfig is safe only if the narrowed set is the whole project;
i.e. it is not safe. It is usable for a *throwaway* answer (an LSP-like live query), not for
splicing into the stored index.

#### scip-python — `--target-only`: right occurrences, wrong paths, wrong scope

```
scip-python index --project-name mypy --project-version 1.0 --target-only mypy/plugins --output T.scip --quiet
```

| Metric | Whole package | `--target-only mypy/plugins` |
|---|---|---|
| Wall | 84.66 s | **19.51 s** |
| Max RSS | 2,947 MB | 923 MB |
| Documents | 1,316 | **82** (only 11 of them are inside `mypy/plugins`) |
| `project_root` | `…/corp/py` | **`…/corp/py/mypy/plugins`** (rebased) |
| Example `relative_path` | `mypy/argmap.py` | **`../argmap.py`** (escapes the project root) |

**MEASURED.** 71 of the 82 documents have a `relative_path` beginning `..`, which
`scip.proto` forbids: "The path must be canonical; it cannot include empty components
('//'), or '.' or '..'". `scip lint` does not catch it.

After normalising the path (join with the rebased root, re-relativise, and exclude
`relative_path` from the hash), **50 of 82 documents are canonically identical** to the
whole-package run and 32 differ. Inspecting one of the 32 (`mypy/applytype.py`):
**occurrences 0 differences**, symbols differ by exactly 2 entries — stub
`SymbolInformation` records for symbols *defined in another document*
(`` `mypy.types`/CallableType#variables. ``) that the full run attached and the target-only
run did not. For `mypy/argmap.py` the full `scip print --json` diff was **0 lines** once
the path was normalised.

**INFERRED:** `--target-only` is a 4.3× speed-up that produces correct occurrence data for
files it emits, but it (a) rebases paths into a spec-violating form, (b) does not restrict
the emitted set to the target, and (c) drops some cross-document symbol metadata. It would
need path rewriting and a symbol-metadata merge before it could feed the importer.

#### scip-java — per-file shards: the only exact per-file mechanism

`scip-java index` leaves a durable shard store:

```
target/scip-targetroot/javacopts.txt
target/scip-targetroot/META-INF/scip/src/main/java/m/{A,B,C,D}.java.scip
```

| Experiment | Result |
|---|---|
| `aggregate --no-parallel --targetroot target/scip-targetroot` | 0.29 s; **all 4 documents byte-identical to what `scip-java index` produced** |
| `aggregate` over a directory containing **only `B.java.scip`** | 0.26 s, 1 document — but every global symbol became `scip-java maven **. .** m/B#` instead of `scip-java maven **maven/m/m 1** m/B#` |
| same, with `javacopts.txt` also copied into the subset directory | **B's document is byte-identical to the full aggregate** |
| edit `B.java`, re-run `scip-java index` | `A/C/D.java.scip` md5s **unchanged**; only `B.java.scip` rewritten; 1 of 4 documents changed |

**MEASURED.** Two findings the upstream docs do not state:

1. A subset aggregate is **exact**, provided the targetroot's non-shard metadata
   (`javacopts.txt`, which carries the classpath from which the Maven coordinates are
   derived) travels with it. Without it, every symbol string loses its package coordinates
   and nothing joins.
2. Even though `scip-java index` forces a clean build
   (`-Dmaven.compiler.useIncrementalCompilation=false`, `clean verify`), the **shards for
   unchanged files are byte-stable**. So "which documents changed" can be answered by
   hashing 4 small shard files instead of decoding and comparing the index.

**Caveat — now MEASURED.** Nothing prunes shards for deleted or renamed sources: the plugin
only ever writes, and `aggregate` reads the *shard directory*, not the source tree. Deleting
`src/main/java/m/D.java`, leaving its shard in place, and aggregating produced an index that
still contains a full `src/main/java/m/D.java` document (3 occurrences, 3 symbols,
`be2b9cca8882446b`) for a file that no longer exists.

The reason this is normally invisible is the other half of the measurement: `scip-java index`
on a Maven project runs **`clean verify`** (4 `clean` mentions in the build log, alongside
`-Dmaven.compiler.useIncrementalCompilation=false`), which deletes the whole targetroot and
regenerates every shard. Deletion is therefore handled correctly by the supported path —
*by throwing away exactly the incremental state Option 2 wants to keep*. Keeping the
targetroot is what creates the eviction duty; the two cannot be separated.

One result in Option 2's favour: shards are **deterministic across clean rebuilds**. A full
`clean verify` regenerated `A/B/C.java.scip` byte-for-byte identical (md5 `186dbcf3…`,
`75c592b9…`, `82c23ba0…` before and after), so shard bytes are a valid change-detection key.

#### scip-clang — filtered compdb: exact for TUs, unstable for ill-behaved headers

```
jq '[.[] | select(.file | endswith("a.c"))]' compile_commands.json > cc_only_a.json
scip-clang --compdb-path cc_only_a.json --index-output-path a.scip
```

| Run | Wall | Documents | vs. full compdb |
|---|---|---|---|
| full (4 TUs) | 0.17 s | 5 (`a.c`,`b.c`,`c.c`,`shared.c`,`shared.h`) | baseline |
| `a.c` only | 0.13 s | 2 (`a.c`, `shared.h`) | **both byte-identical** |
| `a.c` + `b.c` | 0.14 s | 3 | **all three byte-identical** |

**MEASURED.** The shared header is emitted exactly once per run (scip-clang's driver claims
well-behaved headers), and its document is identical across subsets.

Then the hazard, deliberately provoked: a header whose expansion depends on the TU's macros
(`cond.h` guarded by `#ifdef MODE_A`, with `a.c` compiled `-DMODE_A` and `b.c` not):

| Run | `src/cond.h` document hash | occurrences |
|---|---|---|
| full compdb (a,b,c,shared) | `7e0e058255a24829` | 5 |
| `a.c` only (`-DMODE_A`) | `7e0e058255a24829` | 5 |
| `b.c` only (no define) | **`741f96cf059b028d`** | **4** |

**MEASURED.** The header document that lands in the index depends on which TUs were in the
compile database. TU documents (`a.c`, `b.c`) were byte-identical across all runs; only the
conditional header flipped. **INFERRED:** a filtered scip-clang refresh is exact for the
`.c`/`.cc` documents it re-runs, but it must **not** blindly replace header documents —
those belong to the whole-unit run.

#### rust-analyzer — sub-crate paths: exact documents, zero savings

| Run | Wall | Max RSS | Documents | `project_root` |
|---|---|---|---|---|
| `rust-analyzer scip .` (workspace) | 1.56 s | 593 MB | 2 | `…/rsws` |
| `rust-analyzer scip ./crates/beta` | **1.56 s** | 595 MB | 1 (`src/lib.rs`) | `…/rsws/crates/beta` |

**MEASURED.** With `relative_path` excluded from the hash, `beta`'s document is
**byte-identical** in both runs (`5a265da3ae4ae6bf`), and its cross-crate symbol strings are
character-for-character the same (`rust-analyzer cargo alpha 0.1.0 AlphaThing#`,
`… alpha_fn().`). But there is **no time saving at all** — the whole crate graph is still
analysed and the filtering happens at emission (`strip_prefix` fails → document skipped).
Paths are rebased, so a sub-crate index must be re-rooted before it can be spliced.

### 3.5 Is document-level replacement in our SQLite import sound?

Modelled directly with `docswap` on the codectx Go index, then validated with `scip lint`
(whose `missingSymbolForOccurrenceError` is exactly the cross-document integrity check we
care about).

| Index | How built | Dangling `DecodeID()` refs | Documents differing from a full re-index | (aggregate `scip lint` errors) |
|---|---|---|---|---|
| baseline | `scip-go index` | **0** | — | 8,852 |
| full re-index after edit B | `scip-go index` | **0** | 0 (definition) | 8,819 |
| **splice B** | baseline + `internal/model/source.go` replaced from the edit-B index | **0** | **1 of 141** — and that one is `grammars.go`, a known-nondeterministic document | 8,833 |
| **bad splice C** | baseline + **only** `internal/model/hash.go` replaced from the rename index | **46** | — | 8,900 |
| **good splice C** | baseline + **all 8** changed documents replaced | **0** | **2 of 141** — both known-nondeterministic | 8,839 |

**MEASURED.** *Read the third column, not the last one.* The aggregate `scip lint` count is
~8,800 in **every** index including the pristine one, because scip-go does not populate
`external_symbols` for the Go standard library, so every `testing/common#Fatalf().`
occurrence is reported as unresolved everywhere. That floor also means the aggregate drifts
by a few dozen between any two indexes purely because the replaced document's own occurrence
set changed size — which is why splice B (8,833) sits *between* the baseline (8,852) and the
full re-index (8,819) and should not be read as "closer to" or "further from" either. The
meaningful signal is the **dangling-reference delta** on the symbol the experiment actually
renamed, and that one is unambiguous: 0 / 0 / 0 / **46** / 0.

- Splicing the correct document set produces an index that is equal to a full re-index
  except for documents that are not reproducible anyway. **Document-level replacement is
  sound.**
- Splicing an *incomplete* set is detectably unsound: the bad splice added **46 dangling
  `DecodeID()` occurrence errors** (0 in the good splice, 0 in the baseline), exactly at
  the call sites in the 7 files we did not replace.

This is the empirical form of plan §9.4's rule: *cross-file resolution reads are
dependencies*. The replaced-document set must be the set of documents the tool actually
changed, which we can compute cheaply by comparing canonical document hashes between the
stored unit and the fresh index — or, for Java, by comparing shard file bytes.

---

## 4. LSP servers: how they stay incremental, and what they can export

All **SOURCED**.

| Server | One-file edit | Persistent cache | Bulk export we could import |
|---|---|---|---|
| **gopls** | `TextDocumentSyncKind.Incremental`; `DidModifyFiles` → `invalidateViewLocked`; recomputation unit is the **package**, narrowed by the `typerefs` index ("a nearly minimal set of packages that could affect the type checking of P") | **yes** — `$GOPLSCACHE` or `os.UserCacheDir()/gopls/<first 4 bytes of the sha256 of the gopls binary>/…`; 1 GB soft budget, 5-day max age; stores xrefs, methodsets, typerefs, diagnostics and facts; keyed by "a SHA-256 digest **of the recipe of the value**" | **no.** Subcommands are `serve, version, help, api-json, licenses` + feature commands; no `scip`, `lsif`, `index` or `dump`. The cache directory is namespaced by the binary hash *precisely so the format can change freely* — not an external interface |
| **typescript-language-server / tsserver** | Incremental sync; each LSP change becomes a tsserver `Change` command; `IncrementalParser.updateSourceFile` reuses old AST nodes in place; a new `Program` (and therefore a new `TypeChecker`) is built, with `oldProgram` passed for structure reuse | **no.** `--incremental`/`.tsbuildinfo` is a **tsc** feature; grep of `src/server/{project,editorServices,session}.ts` for `tsbuildinfo` → 0 hits. `DocumentRegistry` shares SourceFiles across projects **in memory only** | **no.** `findReferences` is literally O(all files): `findReferencedSymbols(program, …, program.getSourceFiles(), …)` |
| **pyright** | Incremental sync; `Program._markFileDirtyRecursive` walks `importedBy` to invalidate dependents, forcing rebinding when a chained source file changed; phases tokenize → parse → bind → check, and "the checker doesn't run on all files" | **no.** "a **persistent in-memory** object"; no cache directory, no analysis serialization anywhere in `packages/pyright-internal/src` | **no semantic dump.** `--outputjson` emits diagnostics only; `--dependencies` emits the import graph; `--verifytypes`/`--createstub` are coverage/stub tools |
| **clangd** | Incremental sync; per-file `ASTWorker` queue with debounced rebuilds; **preamble reuse** is what makes an edit cheap (`isPreambleCompatible` requires a byte-identical compile command plus Clang's `CanReuse`); `--pch-storage` defaults to `disk` | **yes, and it is the richest.** `.cache/clangd/index/<name>.<hex>.idx` next to `compile_commands.json` (fallback `<user cache>/clangd/index`). **Staleness is content-based**: `digest(buffer) != LS.Digest` decides re-indexing; the hex in the filename is a digest of the *path*, only for disambiguation | **technically yes, practically no.** RIFF container type `CdIx`, sections `meta/srcs/stri/symb/refs`, `constexpr static uint32_t Version = 21;` with "non-current versions are rejected". `--index-file` help says verbatim "**WARNING: This option is experimental only, and will be removed eventually. Don't rely on it**", and a maintainer confirms "the format of the index files **can change between versions**". `clangd-indexer`/`clangd-index-server` exist; re-indexing there is not incremental |
| **jdtls** | Incremental sync; Eclipse `IncrementalImageBuilder` with name-based reference collection (`qualifiedStrings`/`simpleStrings`/`rootStrings`), skipping cascades when a dependency's API did not change (`structurallyChangedTypes`), capped at `MaxCompileLoop = 5` before a full build; `clearLastState()` forces a full build after any failure | **yes** — `<workspace>/.metadata/.plugins/org.eclipse.jdt.core/<crc32 of container path>.index`, one per source folder / project / JAR / JRT, header `INDEX VERSION 1.134`; plus builder `state.dat` with `VERSION = 0x0027`. jdtls adds an opt-in shared library index via `jdt.core.sharedIndexLocation` | **no.** Internal `DiskIndex` format with a hard version check and no spec; the API is `SearchEngine`, a query API |
| **rust-analyzer** | Incremental sync; edits are salsa input writes; "**typing inside a function's body never invalidates global derived data**" | **no.** "The analyzer keeps all this input data **in memory and never does any IO**". Persistent caching is issue #4712, open since 2020 | **yes — `rust-analyzer scip` and `rust-analyzer lsif`.** This is the pattern: same engine, separate batch entry point |

**Cross-cutting.** The premise "workspace/symbol is query-based, not a dump" needs a
correction: LSP 3.17 says "Clients may send an empty string here to request all symbols."
The real obstacles are that servers cap results unilaterally (clangd `--limit-results`
default 100, `--limit-references` default 1000), that the payload carries no references,
types or relations, and that per-symbol `textDocument/references` is O(project) per call in
tsserver. Sourcegraph tried the language-server route in production and abandoned it:
"Language servers served our users well for a number of years, but eventually… **scaling and
performance became an issue**". **SOURCED.**

**What the plan already does is correct and is confirmed by this.** Plan §11.5 uses LSP as
a live, snapshot-qualified overlay and explicitly refuses to make it a second indexing path;
§11.4 uses batch SCIP for canonical facts. Four of the six LSP servers cannot export bulk
facts at all, the fifth (clangd) has an explicitly unstable format with a "don't rely on
it" warning attached, and the sixth (rust-analyzer) exports through exactly the batch
subcommand the plan already pins.

**One concrete addition worth noting:** clangd's background index *is* content-hash
incremental per TU and persists in `.cache/clangd/index/`. If a repository already has a
warm clangd cache, clangd answers C/C++ live queries after an edit without re-running
anything — which makes the LSP-overlay-first policy (Option 3 below) strongest exactly where
the batch indexer is weakest.

---

## 5. Cost of a "changed file → re-run X, replace documents Y" policy

Measured on the corpora above. "Re-run" is the tool cost we cannot avoid; "replace" is what
the importer writes.

Document sizes are the exact serialized bytes of the affected document, measured with
`proto.Marshal(Deterministic: true)`, not an average.

| Unit | Tool cost for one changed file | Documents to replace | Bytes to re-import | Fraction of the index |
|---|---|---|---|---|
| codectx Go module (37.7k lines, 6.5 MB index) | 0.32–0.98 s warm (10.4 s if `GOCACHE` is cold) | 1 of 141 | `internal/bench/doc.go` 511 B (1 occurrence) … `internal/model/source.go` 44,716 B (366) | 0.008 %–0.69 % |
| golang/tools Go module (406k lines, 22.7 MB index) | 0.78–2.12 s warm (7.0 s cold) | 1 of 954 | `go/ast/inspector/inspector.go` 29,393 B (358 occurrences) | 0.13 % |
| TypeScript project (191k lines, 23.2 MB index) | 6.3–9.0 s | 1 of 76 | `src/compiler/core.ts` 523,231 B (4,848 occurrences) — one of the largest files in the project | 2.3 % |
| Python package (1,316 files, 55.6 MB index) | 83–87 s | 1 of 1316 | `mypy/nodes.py` 807,494 B (9,086 occurrences) — again a worst case | 1.5 % |
| Maven module | 2.1 s (full clean build) **or 0.29 s** if only `aggregate` re-runs | 1 of 4; detectable by 1 changed shard file | — | — |
| `compile_commands.json` unit | 0.13 s for 1 filtered TU vs 0.17 s for 4 | 1 TU document (+ headers, which must not be replaced) | — | — |
| Cargo workspace | 1.56 s, identical whether narrowed or not | 1 of 2 | — | — |

A multi-file commit scales linearly in documents, not in tool runs: case C (8 files
renamed in one commit) cost one 0.98 s re-run and 815,361 bytes across 8 of 141
documents — 12.5 % of the index, still an eighth of a full rewrite.

**INFERRED:** with document-level replacement, the marginal cost of a refresh is the tool's
whole-unit wall time plus roughly 0.01 %–2 % of the import. Without it, the refresh rewrites
the entire unit's nodes, relations, evidence, aliases and FTS rows — which on the mypy
corpus is 1,316 documents and 538,538 occurrences instead of one document and 9,086
occurrences, and on codectx is 141 documents and 72,086 occurrences instead of one document
and 366.

---

## 6. Conclusions

### 6.1 Smallest correct re-index granularity, per tool

| Tool | Smallest granularity that is provably exact | Cost vs whole unit | Conditions |
|---|---|---|---|
| **scip-java 0.13.1** | **one source file** (`javac -Xplugin:scip` writes one shard per file; `aggregate` merges a subset) | aggregate 0.29 s vs 2.07 s for a full Maven build | targetroot metadata (`javacopts.txt`) must accompany any shard subset; **measured:** nothing evicts shards for deleted files, and the supported `index` path hides this only by running `clean verify` |
| **scip-clang 0.4.0** | **one translation unit** via a filtered `compile_commands.json` | 0.13 s vs 0.17 s here; linear in TU count on real projects | TU documents are exact; **header documents are not** when the header's expansion is macro-dependent — never replace a header document from a filtered run |
| **scip-go 0.2.7** | **one Go package pattern**, with `--skip-implementations` on both the stored and the refresh run | 0.12–0.24 s vs 0.68 s on 406k lines | 11 of 45,453 occurrences (0.02 %) get a different interface-method descriptor when the defining package is outside the pattern; either cover the closure or repair that one shape |
| **rust-analyzer 1.98** | whole workspace (a sub-crate path is an output filter only) | **no saving** — 1.56 s either way | sub-crate paths are rebased and must be re-rooted |
| **scip-typescript 0.4.0** | whole tsconfig project | narrowing is 5× faster but changes anonymous-type symbol names | `typeLiteralN` names depend on the project file set; not spliceable |
| **scip-python 0.6.6** | whole package tree | `--target-only` is 4.3× faster but rebases paths into a spec-violating `../` form, emits a larger set than the target, and drops some cross-document symbol stubs | would need path rewriting plus a symbol-metadata merge |

### 6.2 Is document-level replacement in our SQLite import sound?

**Yes, under three conditions, all of them measured.**

1. **Replace whole documents, keyed on `relative_path`, with a delete-then-insert.** Never
   append or union: the reference implementations disagree (`FlattenDocuments` unions,
   `expt-convert` keeps the first), and a `Document` carries no recency marker to break the
   tie. Local symbols are document-scoped by spec, so whole-document replacement disposes
   of them correctly; partial replacement would not.
2. **Replace the full set of documents the tool changed**, computed by comparing canonical
   document hashes (or, for Java, shard bytes). The incomplete splice produced 46 dangling
   occurrence errors; the complete one produced none and matched a full re-index.
3. **Treat the fresh index's document set as authoritative, not as a patch.** A deleted
   source file produces **no** `Document` at all — scip-go emits no empty tombstone (case M:
   141 → 139 documents). So the rule is set-based: *paths in the stored unit that are absent
   from the fresh index are deleted*. A pure "replace the paths that changed" loop would
   retain facts for files that no longer exist, and since plan §9.4 makes a rename a new
   FileID, every `git mv` takes this path.
4. **Reconcile the index-scoped state separately**: `Index.external_symbols`, and any
   inverse-relationship table we derive from `SymbolInformation.relationships` (which is
   stored per document but names symbols in other documents, and which Sourcegraph's own
   consumer rebuilds in a whole-index pass). `Metadata` is index-scoped and must come from
   the run, never be merged.

Two corpus-level caveats the plan should absorb:

- **A SCIP global symbol name is not always a unique key.** scip-go gave two distinct
  declarations the same symbol string in 3 of 141 codectx documents (Go's blank `_` at
  package scope; function-local `d`, `_`, `bool`). Plan §9.4 already forbids merging by
  short name; this shows the *native* key can collide too, so `native_aliases` must tolerate
  one `(scope_key, native_key)` mapping to several nodes, and the changed-document test must
  treat these documents as always-dirty rather than as real changes.
- **A small set of documents is not reproducible.** 2 of 141 (codectx) and 3 of 954
  (golang/tools) documents differ between two identical scip-go runs. Any "did this document
  change" check must tolerate that, or we will rewrite those documents on every refresh.
- **`relative_path` is not guaranteed to stay inside the repository.** **MEASURED:** 18 of
  the 141 documents scip-go emits for codectx (798 occurrences, 144 symbols) have paths like
  `../../../../../../../../home/<user>/.cache/go-build/12/12f2159…-d` — the `go test` main
  files, which live in `$GOCACHE` and are content-addressed, so their paths change whenever
  the generating package changes. Splicing these into a path-keyed store would bake absolute
  machine paths into the index, churn ~13 % of the document set for unrelated reasons, and
  break any join against repository `FileID`s. Two mitigations, both verified: the importer
  must reject any document whose `relative_path` escapes `project_root`, and
  `scip-go --skip-tests` removes them outright (141 documents → 104, 18 escaping → **0**).
  The same class of hazard is why §3.4 warns against splicing scip-clang header documents.

### 6.3 Ranked options for the plan

**Option 1 — Importer-level document replacement with a whole-unit re-run. Adopt.**
Keep running the tool over the whole frontend-native unit exactly as §11.4 says, and make
the *importer* diff at document granularity: for each document in the fresh index, compare
its canonical hash against the stored one; reuse unchanged documents' nodes, relations,
evidence, aliases and FTS rows; replace only the rest; and delete the facts for any stored
path the fresh index no longer contains. Drive this off the **hashes, never off git's
changed-file list** — case L showed an untouched file's document changing because an
interface it implements gained a method, and case M showed a deleted file arriving as an
absence rather than as a tombstone.
*Why it wins:* it is the only option that is sound for all six indexers, it needs no
per-tool narrowing logic, and it captures the measured 99 %+ of the import cost. It fits
the current schema without a new concept — a SCIP unit gains an internal per-document
membership so the unit's facts can be partially rebuilt, while the unit remains the
immutable, sealed thing §12.2 requires.
*Trade-off accepted:* the tool's wall time is unchanged. A one-line Python edit still costs
83 s of pyright before anything is written. This option does nothing for latency; it buys
storage, FTS and reconcile cost, and it makes a background refresh cheap enough to run often.

**Option 2 — Java-first true incrementality via the shard store. Adopt after Option 1.**
scip-java is the one tool where the *analysis* is already per-file. Keep the targetroot
between runs (do not clean), let the build tool recompile what it will, then run
`aggregate --parallel` (parallel is safe: document hashes were identical across three runs,
only file byte order varied) and let Option 1's document diff do the rest. On the measured
fixture this turns a 2.07 s Maven build into a 0.29 s aggregate whenever the build tool
itself does nothing.
*Trade-off accepted:* we must evict shards for deleted and renamed sources ourselves. This
is measured, not assumed: aggregating a targetroot whose `D.java.scip` shard outlived its
deleted source produced an index still containing a full `D.java` document. The supported
`scip-java index` path avoids this only because it runs `clean verify` and rebuilds every
shard — which is the cost Option 2 exists to avoid, so the eviction duty is not optional.
We must also carry `javacopts.txt` with any shard subset. Only the **Gradle** build path is
unmeasured here (the Maven path is measured end to end); Task 22 should run it before we
rely on it.

**Option 3 — LSP-first freshness for live edits, background SCIP refresh. Already in the
plan; this round strengthens it.** Plan §11.5's overlay is the right answer to "the user
just edited a file and wants an answer now", because every one of the six servers is
incremental on a single edit while every one of the six batch indexers is not. clangd in
particular has a content-hash-incremental persistent per-TU index, and gopls has a
persistent, recipe-keyed file cache; both answer after an edit without redoing the project.
*Trade-off accepted:* overlay facts stay labelled `language_server` precision and outside
canonical context planning, exactly as §11.5 already requires. No change to the plan is
needed; this round simply confirms the design was right and supplies the evidence.

**Option 4 — Narrowed re-index where the tool allows, as a later optimisation.** Only
scip-go (`--skip-implementations` plus a package pattern covering the changed packages'
closure) and scip-clang (a filtered compdb, replacing TU documents but never header
documents) are exact enough to be spliced. On the 406k-line Go corpus this is 0.24 s vs
0.68 s — a real but modest saving on top of a cost that is already small when `GOCACHE` is
warm.
*Trade-off accepted:* two tool-specific correctness rules, each with a measured exception,
for a saving that only matters on very large units. Not worth doing before Option 1.

**Option 5 — Reject: naive index concatenation, `FlattenDocuments` as a merge, or trusting
`--target-only`/narrowed tsconfig output.** Concatenation silently last-wins on
`Metadata.project_root` and `tool_info`, concatenates `tool_info.arguments`, and violates
the schema's "metadata must appear once" rule with no diagnostic. `FlattenDocuments` keeps
stale occurrences. `--target-only` emits `../` paths the spec forbids. A narrowed tsconfig
renames anonymous type literals. All four are measured, not assumed.

### 6.4 One non-obvious operational note

**Do not let `GOCACHE` go cold.** scip-go's whole-module re-index is 0.29 s warm and 10.4 s
cold on codectx, 0.68 s vs 6.97 s on golang/tools. The plan's private materialization
(§11.4: "a warm Go module cache") should be read to include the *build* cache, not only the
module cache, and the toolchain should not use a fresh `GOCACHE` per run. This is the
single largest incremental lever available for Go, and it lives outside the indexer.

---

## Appendix: exact commands

```bash
# helpers (scratchpad, not shipped)
dochash -mode=hash|meta|dup|occ|doc|parts [-nopath] [-doc=PATH] index.scip
docswap base.scip patch.scip out.scip <relative_path>...

# scip-go
# case L (type change, no text change in the affected file): add UnitID() to provider.Sink,
#   add the method to BatchSink/flushing/recordingSink so it still builds, leave units.go alone
# case M (deletion): rm internal/provider/provider_test.go && scip-go --output go-del.scip
#   -> 141 documents become 139; no document is emitted for the deleted path
scip-go --skip-tests --output go-notest.scip   # 104 docs, 0 with a $GOCACHE-escaping path
scip-go index --output X.scip -q
scip-go index ./internal/storage/... --skip-implementations --output X.scip -q
scip-go missing ./internal/storage/...
GOCACHE=$(mktemp -d) scip-go index --output X.scip -q     # cold-cache measurement

# scip-typescript
node .../scip-typescript index --output X.scip --no-progress-bar src/compiler
# narrowing: a tsconfig.json extending the project base with an explicit "files": [...]

# scip-python (needs pip on PATH; python3 -m venv .venv && PATH=.venv/bin:$PATH)
scip-python index --project-name mypy --project-version 1.0 --output X.scip --quiet
scip-python index ... --target-only mypy/plugins --output X.scip --quiet

# scip-java
java -jar scip-java index --output X.scip
java -jar scip-java aggregate --no-parallel --targetroot target/scip-targetroot --output X.scip
# subset: copy javacopts.txt + the wanted META-INF/scip/**/F.java.scip into a new dir, aggregate that
# deletion: rm src/main/java/m/D.java, leave D.java.scip in the targetroot, aggregate
#   -> the index still contains a full src/main/java/m/D.java document (stale shards are never pruned)
# scip-java index on Maven runs `clean verify`, so it rebuilds every shard byte-identically

# scip-clang
scip-clang --compdb-path compile_commands.json --index-output-path X.scip
jq '[.[] | select(.file | endswith("a.c"))]' compile_commands.json > cc_only_a.json

# rust-analyzer
rust-analyzer scip . --output X.scip
rust-analyzer scip ./crates/beta --output X.scip

# validation
scip lint X.scip | grep -c '^error:'
scip print --json X.scip | jq -S '.documents[] | select(.relative_path=="…")'
```
