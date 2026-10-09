# The library's indexing on the kubernetes export (Oct 8 2026, 5:51 PM to 6:09 PM EDT)
Library 0.26.2, harness tindex (`../harness/source/`). The same 546 613 documents; units are those with at
least one document (47 995). Token counts matched our store's on every document in every run.

| variant (threads, memory budget, commit cadence, tokenizer) | wall | CPU | peak RSS | disk | commits | segments |
|---|---|---|---|---|---|---|
| 16, large (2048 MB), end, unicode61 | 13.1 s | 57.9 s | 1.26 GB | 353.5 MB | 1 | 2 |
| 1, default (50 MB), end, unicode61 | 18.1 s | 26.9 s | 0.49 GB | 337.2 MB | 1 | 13 |
| 16, default, end, unicode61 | 9.0 s | 48.8 s | 0.85 GB | 361.9 MB | 1 | 11 |
| 16, default, every 1000 units, unicode61 | 20.4 s | 70.0 s | 0.86 GB | 343.1 MB | 48 | 12 |
| 16, large, end, code-aware (camelCase parts) | 13.3 s | 67.0 s | 1.36 GB | 386.8 MB | 1 | 2 |
| 1, default, every unit, unicode61 | 954.4 s | 869.1 s | 0.23 GB | 337.4 MB | 47 995 | 16 |

Our engine, same documents: 400.7 s wall, 593.7 s CPU, 2.45 GB peak, about 641 MB live search bytes (01-ours-index.md).
A commit per unit costs about 20 ms each (fsync-bound) and dominates that variant. "end" means one commit after
every document; "-final" includes waiting for the merges. Disk is the index directory as `du -sb` reports it.
