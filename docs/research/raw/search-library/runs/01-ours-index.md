# Our engine's search indexing on the kubernetes export (Oct 8 2026, 5:44 PM to 5:51 PM EDT)
- Harness: tbindex (`../ours/source/`) at main 16dfef6; 16 unit workers (the indexing coordinator's own count);
  one SQLite writer; ingestion batches of 1000 records / 4 MiB, as production.
- Input: docs.jsonl, 546 613 documents in 49 280 units (1 285 units carry no document and are not built).
- Wall: total 400.7 s (6 min 41 s); build 233.7 s; activation (lexical compaction inside it) 165.4 s; seed 1.6 s.
  CPU 593.7 s. Peak RSS 2.45 GB (harness), 2.39 GB (/usr/bin/time, 6:43.29 elapsed).
- Summed over units at 16 workers (not walls): begin 704.9 s, nodes 556.1 s, put 1194.4 s, seal 730.3 s.
  `nodes` and `begin` are rows the store requires for a document to exist, not search work.
- Store: database 3.46 GB, directory 4.2 GB. Search tables 2.90 GB, of which lexical_segment_parts 2.53 GB.
  Live segment bytes (the 7 segments the generation names): 265.4 MB. The remaining 2.12 GB of part bytes belong to
  the per-unit segments that the activation merged away; they stay until a collection pass removes them (this
  harness runs none). The real Sept 27 full run on the same corpus measured lexical_segment_parts at 268.4 MB after
  its retention and collection, which agrees with the live figure.
- Live search bytes: segments 265.4 MB, FTS5 data 148.0 MB, FTS5 docsize 9.6 MB, search_units 163.1 MB plus its
  indexes 54.6 MB: about 641 MB.
- Write amplification: about 2.39 GB of segment parts written to produce 265 MB live (about 9x), each byte also
  passing through the write-ahead log.
