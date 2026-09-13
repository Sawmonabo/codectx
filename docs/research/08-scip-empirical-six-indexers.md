# SCIP empirical verification: six indexers, one fixture shape (2026-09-13, linux/amd64)

Fixture in every language: a global `counter`, `helper(x)`, and `run()` doing
`counter = helper(counter)`, then `f = helper` (function value, no call), then `f(2)`.
Decoded with `scip print --json` (scip CLI v0.10.0).

| Indexer | Version | Source | Call-site `helper` roles | Function-value `helper` roles | Assignment target `counter` roles | Definition roles | typed_range | enclosing_range on defs |
|---|---|---|---|---|---|---|---|---|
| scip-go | 0.2.7 | GitHub release `scip-go-linux-amd64.tar.gz` sha256 5bfe3901… | 8 (ReadAccess) | 8 | 8 (ReadAccess, no WriteAccess) | 1 | no (legacy `range`) | functions only |
| scip-typescript | 0.4.0 | npm | 0 | 0 | 0 | 1 | no | functions only |
| scip-python | 0.6.6 | npm (needs pip on PATH; ran in a venv) | 8 | 8 | 8 | 1 | no | functions only |
| rust-analyzer scip | 1.98.0 (2026-08-18) | rustup component | 0 | 0 | 0 | 1 | yes for file/def | every occurrence carries enclosing_range of its *definition* |
| scip-java | 0.13.1 | GitHub release `scip-java-v0.13.1` (sh launcher + jar, 86 MB) sha256 a694cae1… | absent (0) | absent | absent | 1 | yes (`typed_range`) | fields/methods/locals |
| scip-clang | 0.4.0 | GitHub release `scip-clang-x86_64-linux` sha256 06fd18c5… | 0 | 0 | 0 | 1 | no | none |

## Conclusions
1. **No indexer distinguishes a call site from a non-call reference.** Roles are identical for
   `helper(counter)` and `f = helper`. SCIP alone cannot produce a `calls` edge; it produces a
   resolved reference. The call *site* must come from syntax (tree-sitter `@call.name` range),
   and SCIP resolves the callee occurrence at that exact range. The reviewer's concern is confirmed
   for all six indexers.
2. **No indexer sets WriteAccess (0x4).** `counter = …` is ReadAccess or 0 everywhere. SCIP role
   bits cannot back `reads`/`writes`. Reads/writes need the same join: tree-sitter identifies the
   assignment target (or a later dependence pass does), SCIP resolves what it names.
3. **Range encodings differ per indexer** (legacy `range` vs `typed_range`), which our decoder
   already handles (Task 9 typed_range precedence ruling).
4. **Every indexer needs the project's dependency context**: scip-python requires pip; scip-java
   drives Maven/Gradle and compiled the module; scip-clang needs compile_commands.json;
   rust-analyzer loads cargo metadata; scip-typescript needs tsconfig and node_modules.
5. Wall/RSS on the one-file fixtures: scip-go <1 s; scip-typescript ~1 s; scip-python ~2 s;
   rust-analyzer 2.8 s; scip-java 2.2 s + a Maven build (270 MB RSS); scip-clang 0.1 s.
6. scip-java ships one 86 MB sh-launcher-plus-jar for all platforms and needs a JDK; it ran under
   Temurin 21. scip-clang is linux/darwin only. Everything else has per-platform binaries or npm.
