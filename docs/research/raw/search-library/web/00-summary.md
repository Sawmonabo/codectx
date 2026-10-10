# Web research summary: should tantivy replace codectx's lexical tier?

Eight questions, one file each (01–08). Sources are rated A (peer-reviewed paper, or the engine's own source, javadoc or issue tracker), B (official docs, or a write-up by the team that runs the system), B* (primary research not yet peer-reviewed) or C (secondary, used only to find A/B). Every claim in 01–08 carries its source, year, URL and rating.

Rule labels used throughout: rule 1 exact df over the column union, visible documents only · rule 2 a delta equals a fresh index · rule 3 no top-k cap · rule 4 content-only deterministic ties · rule 5 retained snapshots · rule 6 soft deletes survive merges · rule 7 f64 BM25F, one saturation, weights 5/5/2/2/1, dl = token_count · rule 8 FTS5 unicode61 tokenizer · rule 9 tiers first, then lexical score · rule 10 keyset-cursor paging · rule 11 1e-6 quantization to int64.

## Bottom line

- No engine meets codectx's rules out of the box. tantivy can serve as postings, columns and storage, with codectx's own scoring in a custom collector. That is the only way it stays inside rule 1–rule 7 and rule 11.
- Most of the rules hold up on the evidence. They are justified by identity and repeatability, not by measured ranking quality. Two rules are challenged:
  - Rule 7's document length: codectx weights term frequency by field but not length.
  - Rule 8: camelCase is not split, and the whole snake_case identifier is dropped.
- The evidence specific to code search is thin everywhere. Production teams publish designs and constants, not ablations.

## Verdicts

**1. Field combination (01).** BM25F (sum weighted tf, then saturate once) beats summing per-field BM25.
- The theory is A. Lucene's `CombinedFieldQuery` and Elasticsearch's `combined_fields` were built for exactly that reason (A). Sourcegraph reports about +20% in production code search, on unpublished data (B).
- tantivy has no BM25F. Its multi-field query is the per-field sum, and `DisjunctionMaxQuery` is a different model.
- **Supports** rule 7's one saturation and rule 1's union df. Lucene approximates the union df with the largest per-field df, "too costly" to compute exactly.
- **Contradicts** rule 7's length: the paper and Lucene use a *weighted* length (Σ weight·field length). codectx's `token_count` is the unweighted total, which is FTS5's `bm25()` shape. This is a departure, not a measured loss, and a side-by-side run would settle it.
- Thin: the CIKM 2004 paper itself is closed access; it was read through the authors' own survey and TREC report. No study compares the two field models on code.

**2. Top-k pruning (02).** MaxScore, WAND and block-max WAND are rank-safe for the top k only, and only when every bound truly bounds the score (A, the original papers).
- Real failures:
  - Lucene's WANDScorer missed hits until 8.4 (A).
  - tantivy 0.26.2 scores `DisjunctionMaxQuery` as a sum under block-WAND. The fix, PR #3022, merged on Aug 7, 2026 and is not in a release (A, checked).
  - A tantivy union returned non-matching documents, fixed in 0.26.2 (A).
- Lucene's and Elasticsearch's default hit counts are lower bounds. Complete deep paging needs a cursor over a total order on a pinned view: Elasticsearch has `search_after` with a point in time and `_shard_doc`, and Lucene has `searchAfter`. tantivy has only an offset, and its ties fall to `DocAddress`.
- **Supports** rule 3, rule 4, rule 5, rule 10 and rule 11: score unpruned and outside tantivy's pruning path.

**3. Deleted documents in statistics (03).** Lucene, Elasticsearch and tantivy all count deleted documents in df, N and the token totals until a merge purges them. They say this is deliberate (A).
- Scores jump after a merge, and copies of the same data rank differently (B).
- Elasticsearch's DFS pass and Solr's ExactStatsCache make statistics global across shards, but those statistics still include deletes.
- **No source measures the quality effect.** Distributed-IR work (SIGIR 1995, IP&M 2008; abstracts only) finds ranking robust to approximate statistics.
- **Supports** rule 1, rule 2 and rule 7, on reproducibility grounds, not quality. Rule 6 needs visibility kept outside tantivy, which has no soft-delete feature.

**4. Norm quantization (04).** tantivy's one-byte length table matches Lucene 7+'s on all 256 entries (A): exact up to 40, then up to about 11% low.
- ECIR 2020 (Kamphuis et al.; A) found exact lengths make no significant difference on newswire. The encoding before Lucene 7 cost about 0.005 MAP on one collection (RIGOR 2015).
- **Supports** keeping the exact `token_count` (rule 7, rule 2), on identity grounds: rounded lengths add ties, and tantivy's merge re-estimates avgdl from them. It does not support it on quality grounds.

**5. Score precision and determinism (05).** tantivy is f32 end to end (A). Lucene scores are float, with some internal arithmetic moved to double for correctness (A).
- f32 steps are 1.9e-6 once a score reaches 16, so f32 cannot hold a 1e-6 grid.
- Engines break ties on internal ids (doc id, shard index, `DocAddress`), which change between builds. Elasticsearch requires a unique tie-break key for paging (B).
- **Supports** rule 11, rule 4, rule 10 and rule 7's f64.
- Probe note: Go's `math.Log` and Rust's `f64::ln` differ in the last bit on 1 891 of 200 127 inputs, so the log implementation must be pinned.
- Thin: no study compares f32 with f64 for ranking quality.

**6. Code-aware tokenization (06).** Camel and underscore splitting is accurate (EMSE 2014; A). A smarter splitter adds little over camelCase splitting (ICPC 2011; A).
- The one direct BM25 ablation is a May 2026 preprint (B*) on natural-language queries. Keeping the whole identifier *and* its parts raised NDCG@10 from 0.309 to 0.563 on Go, +27.8% on Java and +22.5% on Ruby (checked against the paper). Parts only scored 0.469 on Go.
- Production practice: GitHub Blackbird, Google Code Search, Zoekt and livegrep use substring indexes. OpenGrok and GitHub's old Elasticsearch index kept whole identifiers.
- **Partly contradicts** rule 8. No stemming is supported. Not splitting camelCase, and dropping the whole snake_case form, go against the best (thin) evidence.
- Candidate rule: emit the whole identifier plus its parts at the same position. That is a custom filter in either tantivy or FTS5, so it does not decide the engine.

**7. Engine landscape (07).** The only reproducible multi-engine benchmark is the Search Benchmark Game: one field, read-only, query latency only, English Wikipedia (A, run by the tantivy team).
- Lucene 10.3 now beats tantivy 0.25 on top-k. tantivy is faster on `TOP_100_COUNT`, the mode that visits every match.
- No reproducible comparison exists against FTS5, Bleve, Bluge, Xapian, Zoekt or Meilisearch, and no benchmark uses a code corpus.
- **No engine looks better than tantivy** for a complete, exact BM25 tier:
  Lucene needs a JVM; Xapian keeps two revisions (breaks rule 5); Meilisearch caps hits (breaks rule 3); Bluge is unmaintained; Zoekt is a different class (substring).
- Bleve (pure Go) is the only alternative worth a benchmark arm, but it scores per field and has no published performance evidence. The incumbent FTS5 stays a valid baseline.
- Embedding:
  - A cgo call costs about 30–40 ns. Milvus and Anytype ship tantivy through cgo.
  - Return one packed buffer per page, never one call per hit.
  - A Rust panic aborts the process, so use `catch_unwind` at every export or the existing worker process.

**8. Ranking signals (08).** Definition, file-name and whole-token boosts and test, vendor and generated-file demotion are universal in production (Google, GitHub, Zoekt, Sourcegraph; B), but almost never measured.
- One study measured them only bundled with a graph rank: AUC 0.736 → 0.841, on 25 queries (A).
- PageRank added nothing in one learning-to-rank ablation (EMSE 2017; A).
- Proximity has no code-search measurement.
- **Supports** rule 9 and rule 7's form. (07 rates rule 9 neutral: Zoekt and GitHub fold these signals into the score, not into tiers.)
- To measure, not adopt: test, vendor and generated demotion (a gap: codectx has none); Zoekt's constant idf, against rule 1's idf; symbol-kind boosts.
- Any query-independent signal must be stored per document to keep rule 2.

## What would settle the open points

On a judged code query set: weighted against unweighted length (Q1); whole identifier plus parts against unicode61 (Q6); test-path demotion and constant idf (Q8); a cgo packed page against a sidecar on whole answers (Q7).
