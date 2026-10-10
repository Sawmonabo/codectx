# Export of the kubernetes search documents (Oct 8 2026, 5:24 PM to 5:44 PM EDT)
- Corpus: kubernetes at commit dfd7b93a1783878be367e1fc4a780318330cb3bf (Sept 26 2026), 31 407 files.
- Binary: codectx at main 16dfef6 plus the export observer (`../ours/source/`), run as the real `codectx index`
  with scip, lsp and dependence disabled by a bench-only user configuration.
- Run: ok in 19m25s; build 14m25s; filesystem pass 2m47s (writer busy 0.96); tree-sitter pass 11m31s (writer busy
  0.99, worker CPU 55.6 s); plan 5.2 s; peak RSS 2.89 GB (/usr/bin/time).
- Documents: 546 613 (filesystem 37 080, tree-sitter 509 533) in 49 280 units; 437 001 distinct terms.
  With the 15 459 manifest documents the export leaves out, the total is 562 072, exactly the stored count of the
  Sept 27 full run on the same commit: the export is complete.
- docs.jsonl 647 MB; terms.tsv 7.9 MB.
