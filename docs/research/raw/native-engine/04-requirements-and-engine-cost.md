# Native-engine research, raw evidence — the product requirements a native engine must meet, and what the engine costs to meet them

Source: `docs/providers-dependence.md` §Memory and §Units; `internal/model/facts.go:94-98`;
`docs/research/00-synthesis.md` §8 (the adopted direction ruling of 2026-09-13).

## Precision vocabulary (internal/model/facts.go:94-98)
```go

const (
	PrecisionCompiler       Precision = "compiler"
	PrecisionLanguageServer Precision = "language_server"
	PrecisionStaticAnalysis Precision = "static_analysis"
	PrecisionSyntax         Precision = "syntax"
	PrecisionHeuristic      Precision = "heuristic"
```
The engine stamps `static_analysis`. A tree-sitter-derived CFG/CDG/def-use has an exact CFG half
and a name-resolved half; the honest label for the replacement is `syntax` unless the call endpoints
come from a precise index, in which case the *calls* endpoint is `compiler` and the dependence edge
is still `syntax`. This is a product-visible downgrade and is the single largest non-engineering cost.

## What the engine costs to satisfy 'no caps, no subdivision'

| mechanism the engine forces | where | native-engine equivalent |
|---|---|---|
| per-unit heap cap sized from unit source bytes | providers-dependence.md §Memory | none — memory is one function at a time |
| per-family resident allowance C/C++ 2.6 GB, Python 1.9 GB, JS/TS 0.3 GB + 1.7 GB helper on a 4,984-file project | providers-dependence.md §Memory; 10-round3 §10 | none — no JVM, no per-language helper process |
| `max_concurrent_heavy_analyzers = 1` (default), reservations serialise units | providers-dependence.md §Memory | parallel across cores; the unit of work is a function |
| one OOM retry at the machine-derived allocation | providers-dependence.md §Memory | not reachable |
| subdivision as last-resort crash recovery, `partial: subdivided` | providers-dependence.md §Subdivision; 00-synthesis §8 | a crash is one function, not a project |
| unit = frontend-native project; JS/TS never split (46% of resolved calls die if it is) | providers-dependence.md §Units; 10-round3 §8 | unit = file for dependence; project only for calls |
| CSV export of the whole graph, then import | providers-dependence.md §The engine | facts stream straight into the store |

## The direction ruling this report is written under (00-synthesis.md §8, 2026-09-13)
```
## 8. Direction ruling (2026-09-13, user-adopted reviewer directive)
Engine-backed `dependence` is the MVP backend. No default memory ceiling: estimates schedule and
serialize work (`max_concurrent_heavy_analyzers = 1` by default, co-schedule only when summed
reservations fit); only an explicit user limit rejects work up front; one OOM retry at the
machine-derived allocation, only when it exceeds the failed cap. Subdivision is the last-resort
recovery from a reproducible engine crash (confirmed by one rerun with the frontend's fixed,
currently empty, semantics-neutral option allowlist), never for memory, published per capability as
`partial: subdivided` with failed unit, backend failure and affected capabilities. No lowered
analysis limits or omitted fact families. Every advertised language (nine languages, six
frontends; C++ and TSX still to be exercised) runs end to end in Task 22 with real tools. Benchmark
corpora are pinned by commit in Task 21 as the differential oracle for any future native engine.

```
The ruling stands. Nothing in the post-MVP plan is to be started before the MVP ships; the pinned
benchmark corpora named in that ruling are the differential oracle it names.

## The failure-class machinery that exists only because the engine is an opaque subprocess

`docs/providers-dependence.md` §Failure classes — every one reproduced against the real engine:

| class | signal the product has to reverse-engineer | native equivalent |
|---|---|---|
| `memory` | `OutOfMemoryError` substring on a bounded stderr tail, ordered *before* the helper-crash test because an OOM stderr also carries the exit line | not reachable: a function-sized working set |
| `engine` (pass crash) | `Pass <name> failed in <n> ms` at WARN with the throwable, log level pinned in the child environment so the host cannot break classification | a panic in one function, contained and named in-process |
| `engine` (zero-exit helper crash) | "the exit status is a lie in this mode; the empty result is the only honest signal" | not reachable |
| `empty_export` | both steps exit 0 and the export holds no method, because a frontend default excluded every source directory (the Java frontend excludes any path with a `test` component) | not reachable: the product chooses the files |
| `timeout` | the step exceeded the unit deadline | per-function bound |
| definition-cap skip | paired `has more than <n> definitions` / `Skipping.` WARN lines parsed out of stderr | a bound the product itself sets and reports |

Plus `observed_peak_bytes` (tree-summed RSS sampled every 250 ms), `stderr_tail` with every
absolute path redacted, and the `unsupported_labels` pseudo-capability for export rows the importer
recognised but does not map. None of this is accidental complexity in the product — it is the
irreducible cost of consuming a large JVM program through a CSV file and a stderr stream.

## The import staging database — the largest single file the product writes, and why it exists

Source: `docs/providers-dependence.md` §The staging database; ADR-0009.

- The importer stages **the whole export** in a private SQLite database under
  `<data_dir>/dependence/scratch/`, and derives the unit's facts from it by ordered query.
- It is a bulk load: append-only, no secondary index during load, every later ordering built
  afterwards in one pass of the engine's external merge sort.
- Disk traffic is **6.6× the export** (down from 34.5× before the redesign).
- `providers.dependence.staging_cache_kib` defaults to **256 MiB**.
- It is "the largest single file the product writes", held in a shared scratch pool with an
  exclusive lock, with its own retirement rule for a surface a crash left mid-write.
- It cannot simply be deleted per unit: "where the filesystem discards freed blocks and the
  machine's disk is a sparse image, a free of several gigabytes stalls every process on the machine
  about a minute later, unobservably" — the paced reclaimer exists for this.

**The whole subsystem is a consequence of the export format.** The engine hands the product
gigabytes of Neo4j CSV (2.02 GB for postgres, 4.95 GB for a 1.05M-line Python tree, `10-round3` §4)
and the product must turn that into facts without holding it in heap. A native engine produces facts
**in projection order, in process, one function at a time**, so there is nothing to stage: the CSV,
the staging database, its 256 MiB cache, its pool, its lock, its retirement rule and the paced
reclamation of its freed gigabytes all have no native equivalent.

Accounting: `internal/provider/dependence/neo4jcsv` is 3,446 non-test Go lines + 1,428 test lines
= 4,874 lines of package source (plus 99 lines of Go fixture source under its `testdata/` tree). A
native engine **deletes** it, except for the 339-line fact-key comparator, which relocates because it
is what the differential oracle is built on. The honest credit is therefore **4,535 lines**, and it
belongs on the credit side of any effort estimate (`15-requirements-audit.md`).
