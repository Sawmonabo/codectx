# 07 — Every departure from production (and from the frozen contract)

All file:line references are to commit 16dfef6.

1. **Worktree edits.** The harness adds an observer with four nil-guarded hook sites in `internal/storage/sqlite/units.go` (`source/internal/storage/sqlite/units.go.diff`), plus `tbcapture.go`, `internal/search/tbench.go`, `internal/tbench/` and `cmd/tb*`. With no observer installed, each hook costs one nil check on the measured path. The sqlite and search package tests were not re-run on the edited tree.
2. **Forced rows** (see 03). The real snapshot, blobs, builds and inputs are copied. Only the referenced node facts are written, each with one evidence row. All of these are timed apart from search work.
3. **No unrelated writes.** The real run's relations, aliases, other nodes and other providers' units are not written, so the store's one writer is less contended than in a real index.
4. **Exported providers.** Only filesystem and treesitter documents are exported, per the brief; manifest documents (139 on the smoke) are left out. The real run itself ran every default provider, the dependence provider included; on a large repository that provider dominates the exporter's time and memory.
5. **Concurrent phase timings.** At more than one worker, begin, nodes, put and seal are sums over units; `build` is the wall time. The store's ingestion group commits when it chooses, so a commit's cost lands in whichever call triggers it.
6. **Activation inputs.** `Activate` is passed an empty capability list, where production passes the run's capability states (`internal/index/generation.go:482`). The `activation` phase also includes a `Store.Flush`.
7. **Query arm, omitted from `Search`:** the exact tiers (`internal/search/search.go:666`), the final `reader.Capabilities` (`:263`), and the page and item `Validate` calls (`:267`).
8. **Query arm, contract fields.** `page` and `hits` are document-level and not deduplicated, so the arms can be compared; production's deduplicated answer is in `served_hits` and `served_page`. "Cold" means a fresh process on a warm OS page cache.
9. **Path filter.** `--path-prefix` exposes `SearchRequest.Paths`, which the CLI does not expose; the MCP search tool does.
10. **Delta** (see 05). Every unit the edited file feeds is rebuilt, two on the smoke. Search keys are kept. The new blob is synthetic, so the edited file cannot be hydrated after the delta.
11. **Results files.** They are `ours-index.jsonl`, `ours-query.jsonl` and `ours-delta.jsonl`, and they carry extra fields: `workers`, `summed_over_units`, `served_*`, `open_ms` and `filter`.
12. **Smoke repository.** It is spf13/cobra (public, 66 tracked files), cloned for this purpose because no small public repository was otherwise available.
