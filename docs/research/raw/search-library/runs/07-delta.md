# Delta arms on the kubernetes export (Oct 8 2026) — partial (owner ruling, Oct 8)
Harness `tdelta` (library 0.26.2, `target-next`), each on its own `cp -a` copy of t16-large-end (2 segments,
546 613 documents), under `systemd-run --user --scope -p MemoryMax=12G` from a clean environment, one at a time.
Unit: `treesitter/file:vendor/golang.org/x/sys/windows/zerrors_windows.go`, the unit with the most documents
(9 460), the one our engine's delta arm picks by default. tdelta deletes the unit, re-adds its 9 460 documents with
the first one's body changed (" deltachange1"), commits, and waits for merges. Commands, exit codes and output:
`r2-logs/c-audit.log`; results `<bench>/results/tantivy-delta*-copy<X>.jsonl`.

| copy | route | put ms | commit ms | delta (put + commit) ms | merge wait ms | after: segments / deleted docs | process peak RSS MB |
|---|---|---|---|---|---|---|---|
| A | delete_term(unit) | 34.5 | 242.4 | 276.8 | 0.2 | 3 / 9 460 | 71 |
| B | delete_term(unit), then forced merge | 33.9 | 232.0 | 265.9 | 0.3 | 3 / 9 460, then 1 / 0 after a 12 372 ms merge | 71 (merge 403) |
| C | delete_term(id) × 9 460 | 25.9 | 367.0 | 392.9 | 1.3 | 3 / 9 460 | 107 |
| D | delete_query(TermQuery unit) | 6.0 | 312.1 | 318.1 | 0.2 | 3 / 9 460 | 77 |
| E | soft: nothing deleted, 9 460 old addresses written for `--hide` | 6.6 | 292.6 | 299.2 | 0.3 | 3 / 0 | 75 |
| F | delete_term(unit), merge policy log:del=0.2 | 6.4 | 327.8 | 334.2 | 0.3 | 3 / 9 460 (no merge triggered) | 77 |
- Every route costs about 0.27-0.39 s, dominated by the commit (fsync and the new segment's serialization).
  Single runs; A and B, which do the same work, differ by 11 ms.
- The put of A and B (34 ms) is higher than D-F (6 ms) with the same one-delete route; single runs, not explained.
- Peak RSS is the whole process, including reading docs.jsonl for the unit.
- A forced merge to drop the deleted documents from the statistics costs 12.4 s on this corpus.

## Never ran (owner ruling, Oct 8)
- Our engine's delta (`tbdelta -unit <same unit> <bench>/docs.jsonl <bench>/store`): not run, so no like-for-like comparison
  exists yet. Round 1 has no kubernetes delta for our engine either. When it runs, note: our engine rebuilds both units
  the edited file feeds (the treesitter unit above plus `filesystem/file:<same path>`, 30 documents), and edits the
  first document with a non-empty body, where tdelta edits the unit's first document.
- The soft delta's pages (`tquery --hide` on copy E against the hard delete on copy A).
