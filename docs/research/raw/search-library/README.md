# Raw evidence for research 22, the search-library evaluation

The measurement record behind [`../../22-search-library-evaluation.md`](../../22-search-library-evaluation.md) and
[ADR-0013](../../../adr/ADR-0013-search-index.md). Everything ran on Oct 8, 2026, between 5:24 PM and 8:53 PM
(America/New_York), on the kubernetes export described in `runs/00-export.md`. The library is tantivy 0.26.2.

Two arms were measured on the same 546 613 search documents:
- **our arm**: codectx's current lexical tier, driven through its real write and query paths (`ours/`);
- **library arm**: the same documents in the search library, queried three ways: `library` (the library's own BM25
  and pruned top 200), `library-all` (the library's BM25, every match scored) and `product` (our BM25F, statistics,
  fold and comparator, in a custom collector) (`harness/`).

## Layout

| Folder | What it holds |
|---|---|
| `runs/` | The kubernetes runs: export, both index builds, search speed, quality. Logs are raw. |
| `ours/` | How our arm reproduces production (notes 01–07) and its harness source (`ours/source/`). |
| `harness/` | The library arm: its summary (00), notes 01–12 and its harness source (`harness/source/`). |
| `features/` | The library's features, read from its source, with long quotes (01–09). |
| `features-matrix/` | The same features as one matrix (00) and the matrix pass's notes (10–18). |
| `web/` | Literature and engine research, eight questions (01–08) and a summary (00). |

## Edits made when publishing

- **Placeholders.** `<bench>` stands for the measurement's working directory on the host, and
  `<checkout>/kubernetes` for the kubernetes checkout. Every absolute host path in the logs, `/usr/bin/time`
  reports and notes was replaced with one of the two. Nothing else in those files changed.
- **Arm names.** The two arms are called "our arm" and "library arm" throughout. The matrix column that marks what
  the library arm uses is headed "library arm".
- **Numbering.** Every note has its own number. `features/` holds the long feature notes, 01–09 (its 01–04 were a
  subfolder, `indexing/`). `features-matrix/` holds the matrix (00) and its notes, renumbered 10–18. Byte-identical
  copies of 10–18 that sat in `features/` were dropped. Cross-references, two source comments
  (`harness/source/src/library.rs`, `harness/source/src/variants.rs`) and one in `ours/source/internal/tbench/tbench.go`
  were updated to the new paths. The two Go probes under `harness/source/probes/` were run through `gofmt`; no code
  changed.
- **Labels.** The web notes number codectx's product rules as "rule 1" to "rule 11" (listed in `web/00-summary.md`).
  The matrix's search-runtime features are RT1 to RT7.
- **Split log.** The library arm's query log is published as `runs/tquery-speed.log` (speed, filters, pages, explain,
  top-k checks) and `runs/tquery-quality.log` (the quality sets). Concatenated, they are byte-identical to the
  original log; it was split to keep each file under 2 MB.
- **A separate module.** `ours/source/go.mod` makes the our-arm sources their own module, so the repository's
  `./...` never compiles them. They import codectx's internal packages at 16dfef6 and build only inside such a tree.
- **Derived tables.** `runs/03-pooled-latency.txt` and `runs/03-first-page.txt` were computed by `runs/03-derive.py`
  from the per-run result lines, which are not published (see below).
- `runs/host-load.tsv` is the host's load average and free memory every 5 s, copied while later runs were going.

## Not published, and why

| What | Why |
|---|---|
| `docs.jsonl`, the export (647 MB, 546 613 lines) | over 2 MB; `runs/00-export.md` and `ours/01-export-path.md` say how to regenerate it from kubernetes at the stated commit |
| `terms.tsv` (7.9 MB) | over 2 MB; a by-product of the export |
| `runs/export.out` (34 MB), the export's full log | over 2 MB; its summary is `runs/00-export.md` |
| the per-run result lines, `results/*.jsonl` (2.5 GB in all; each speed file holds 200 page hashes per run) | over 2 MB; the logs in `runs/` carry every timing, and `runs/03-derive.py` names the files it reads |
| the indexes, the SQLite store, the quality query and judgment files | build outputs; `harness/07-quality-evaluation.md` gives the seed and the generator that rebuild the sets |
| `target*` build directories and binaries | build outputs |

## Still being measured

The variant runs on kubernetes (exact top-k with our formula, per-segment search threads, our scoring with the
code-aware tokenizer, weighted document length, the visibility routes), the rule audit and the delta runs are
running. Their records will be `runs/05-variants.md`, `runs/06-rule-audit.md` and `runs/07-delta.md`.
