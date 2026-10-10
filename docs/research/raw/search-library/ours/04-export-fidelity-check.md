# 04 — The export's fidelity check

## Self-check printed by the exporter
On exit, the exporter prints the document count per provider and per kind, the number of distinct terms, and the per-provider count of documents it did not export. Smoke on spf13/cobra (public, 66 tracked files), Oct 8, 2026:

```
self-check: 1083 documents in 102 units, 4048 distinct terms
  provider filesystem 93 | provider treesitter 990
  kind build_target 1, class 10, configuration 2, constant 57, document 18, field 176, file 72,
       function 169, interface 1, method 169, struct 14, test 285, variable 109
  not exported (other providers) manifest 139
```

The real store holds 1222 search documents (93 + 139 + 990). The 562 072 documents of the stored large-repository run may include manifest documents, so compare it with the per-provider lines, not the total.

## Index-arm store against the real run's store
The same SQL ran against both databases. It counts rows, sums `token_count`, counts distinct search keys and sums `length(name)+length(path)`, over the filesystem and treesitter units of the active generation:

```
real codectx index store : 1083|102438|1083|33025
tbindex store            : 1083|102438|1083|33025
docs.jsonl (python sum)  : 1083 102438
```

The match holds at 16 workers and at 1 worker.

## Query-level consistency
For a single-term query, the matched-document count equals that term's document frequency in `terms.tsv`. On the smoke: "go" 1052 = df 1052, and "cobra" 982 = df 982. The tier's matching and the terms.tsv tokenizer therefore agree.

Before any delta, every served page was fully hydrated from the real content store (`page_without_range` 0 for every query).
