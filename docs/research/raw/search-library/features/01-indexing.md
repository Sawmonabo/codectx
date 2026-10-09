# 01 — Indexing: the writer, its threads and memory, commits, deletes and merges (tantivy 0.26.2)

Sources: the 0.26.2 crate source (`tantivy-0.26.2/src/...`, `tantivy-stacker-0.7.0/src/...`), its `ARCHITECTURE.md` and `CHANGELOG.md` (the crate ships the files of the 0.26.2 tag), and https://docs.rs/tantivy/0.26.2/tantivy/indexer/struct.IndexWriter.html (fetched; it lists 15 public methods, matching the source). Measured numbers come from the bench arm's own note `../harness/04-commit-cadence.md`.

**codectx context used below.** Each document has five tokenized columns (name, qualified_name, signature, path, body) plus kind, path, id and token_count. Production seals each unit (one provider scope) and activates the build once at the end. Product rules: complete answers (no top-k cap), deterministic ranking, and retained snapshots in which deleted documents stay soft-deleted.

**Harness vocabulary used in the variants.** `tindex <docs> <dir> <threads> [default|large|MB] [unit|every:K|end] [--tokenizer unicode61|code]`. Here `default` = 50 MB per thread, `large` = 2 048 MB in total, and `MB` = an explicit total. A knob with no flag yet is written as an exact setting followed by "needs a harness flag".

---

## 1. Writer construction: thread count and memory budget

**What it is.** An `IndexWriter` runs N indexing worker threads. Each worker builds its own in-memory segment, and all workers read from one shared bounded queue. The memory budget is set per worker.

**Exact API.**
- `Index::writer(memory_budget_in_bytes: usize)` (`tantivy-0.26.2/src/index/index.rs:613`)
- `Index::writer_with_num_threads(num_threads: usize, overall_memory_budget_in_bytes: usize)` (`src/index/index.rs:580`)
- `Index::writer_with_options(IndexWriterOptions)` (`src/index/index.rs:539`), with `IndexWriterOptions::builder().memory_budget_per_thread(..).num_worker_threads(..).num_merge_threads(..).build()` (`src/indexer/index_writer.rs:50-63`)

**Behaviour.**
- **Defaults.** The builder defaults are `memory_budget_per_thread = MEMORY_BUDGET_NUM_BYTES_MIN`, `num_worker_threads = 1` (`index_writer.rs:57`) and `num_merge_threads = 4` (`index_writer.rs:60`).
- **Minimum budget.** `MEMORY_BUDGET_NUM_BYTES_MIN = MARGIN_IN_BYTES * 15` = **15 000 000 bytes per thread** (decimal bytes, not MiB) (`index_writer.rs:29,32`). The source comment says: "We impose the memory per thread to be at least 15 MB, as the baseline consumption is 12MB." The minimum was raised from 3 MB to 15 MB in 0.21.1 "to avoid single doc segments" (CHANGELOG, 0.21.1).
- **Maximum budget.** `MEMORY_BUDGET_NUM_BYTES_MAX = u32::MAX - MARGIN_IN_BYTES`, about 4.29 GB per thread (`index_writer.rs:33`). A budget outside [min, max), or 0 threads, returns `TantivyError::InvalidArgument` (`index_writer.rs:285-301`). The ceiling comes from the arena: addresses are `u32` (12-bit page id plus 20-bit offset, with 1 MB pages, `tantivy-stacker-0.7.0/src/memory_arena.rs:27-28,199-204`), so one worker's arena cannot exceed about 4 GB.
- **The 8-thread cap is only on `Index::writer()`.** It uses `min(available_parallelism, MAX_NUM_THREAD = 8)` (`index.rs:617`, `index_writer.rs:36`), and it lowers the thread count when budget ÷ threads would fall under 15 MB (`index.rs:619-621`). `writer_with_num_threads` and `writer_with_options` have **no cap**: the harness ran 16 threads.
- **The total budget is split evenly.** `writer_with_num_threads` divides the total: `overall_memory_budget_in_bytes / num_threads` (`index.rs:585`). So `large` (2 048 MB) at 16 threads gives 128 MB per thread.
- **When a worker flushes.** After each batch, a worker flushes its segment once `mem_usage >= memory_budget - MARGIN_IN_BYTES`, that is budget − 1 MB (`index_writer.rs:195`). `mem_usage` counts the term arena, fieldnorm writers, fast-field writers and the serializer buffer (`src/indexer/segment_writer.rs:140-145`).
- **Hash table size.** The term hash table starts at the largest power of two whose size is under budget/3, capped at 2^19 entries (`segment_writer.rs:27-41`). Each entry is 8 bytes (a `u32` address plus a `u32` hash, `tantivy-stacker-0.7.0/src/shared_arena_hashmap.rs:25-28`), so the table starts at no more than 4 MiB. The arena itself grows 1 MB page by page.
- **The budget is not RSS.** The queue holds up to `PIPELINE_MAX_SIZE_IN_DOCS = 10_000` documents, and `add_document` blocks when it is full (`index_writer.rs:40`). Merge threads and the reader add memory on top. Measured: 16 × 50 MB per unit peaked at 486 MB RSS, and 1 × 2 048 MB committed once peaked at 30 MB (`../harness/04-commit-cadence.md`). An oversized budget is not preallocated.
- **Thread count and determinism.** DocIds are assigned in the order documents arrive at a worker (ARCHITECTURE.md, "DocId"). With more than one worker, which segment a document lands in depends on thread scheduling. The source's own test helper says: "Using a single thread gives us a deterministic allocation of DocId" (`index.rs:596`).

**Applies to codectx?** Yes. This is the main index-time and index-memory knob. Ranking stays deterministic only if ties break on the stored `id` and never on `DocAddress` (see §7).

**Metric.** Index time and index memory (peak RSS). Indirectly, segment count, which feeds query latency.

**Benchmark variants.**
- Thread sweep at a fixed per-thread budget: `tindex docs.jsonl ix-tT T <50×T> end` for T ∈ {1, 2, 4, 8, 16}.
- Minimum-budget probe: `tindex docs.jsonl ix-min 1 15 end` (exactly 15 MB, the floor). Expect more flushes, so more segments before merging.
- Large per-thread arena: `tindex docs.jsonl ix-big T 2048 end` for T ∈ {1, 4, 8}. That is 2 048, 512 and 256 MB per thread.
- Record wall time, CPU time, peak RSS and segments at end. The `-final` phase must include `wait_merging_threads` (§6).

---

## 2. Adding documents, opstamps and batches

**What it is.** Every operation (add, delete, commit) gets a monotonically increasing `u64` opstamp from a shared atomic counter (`src/indexer/stamper.rs:12-37`).

**Exact API.**
- `add_document(&self, D) -> Result<Opstamp>` (`index_writer.rs:727`)
- `run<I: IntoIterator<Item = UserOperation<D>>>(&self, I) -> Result<Opstamp>` where `I::IntoIter: ExactSizeIterator` (`index_writer.rs:762`)
- `UserOperation::{Add(D), Delete(Term)}` (`src/indexer/operation.rs:27-32`)
- `commit_opstamp(&self) -> Opstamp` (`index_writer.rs:716`)

**Behaviour.**
- **`add_document`** takes one stamp and sends a one-document batch. It may block when the 10 000-document queue is full (`index_writer.rs:727-731`).
- **`run`** reserves `count + 1` contiguous stamps. The last one is the batch's own and is what `run` returns. "add operations of the same group are flushed into the same segment" (`index_writer.rs:746-799`). Deletes inside a batch become `TermQuery(term, Basic)` weights on the delete queue, in order (`index_writer.rs:778-787`).
- **Opstamps survive restarts.** A new writer resumes from `meta.json`'s `opstamp` (`index_writer.rs:308`).

**Applies to codectx?** Yes. One unit maps naturally to one `run` batch: its documents get contiguous opstamps and stay in one segment. A delete-then-re-add of a unit inside one `run` is ordered correctly by opstamp (§4).

**Metric.** Index time: batching cuts per-document channel sends. Segment locality: a unit never splits across workers.

**Benchmark variant.** Per-unit batching vs per-document sends: `run(vec![Add..])` per unit vs `add_document` per document, at `tindex … 8 large end`. Needs a harness flag (`--batch unit|doc`).

---

## 3. Commit, prepare_commit, payload, rollback and what a commit costs

**What it is.** Nothing is searchable or durable until a commit. A commit is two-phase: `prepare_commit` flushes, and then `PreparedCommit::commit` publishes.

**Exact API.**
- `IndexWriter::commit(&mut self) -> Result<Opstamp>` = `prepare_commit()?.commit()` (`index_writer.rs:664-666`)
- `IndexWriter::prepare_commit(&mut self) -> Result<PreparedCommit<'_, D>>` (`index_writer.rs:618`)
- `PreparedCommit::{opstamp(), set_payload(&str), abort(), commit(), commit_future()}` (`src/indexer/prepared_commit.rs:22,27,32,38,47`)
- `IndexWriter::rollback(&mut self) -> Result<Opstamp>` (`index_writer.rs:564`)
- `IndexWriter::delete_all_documents(&self) -> Result<Opstamp>` (`index_writer.rs:522`)
- Reading the payload back: `Index::load_metas()?.payload` (`IndexMeta.payload: Option<String>` is public, `src/index/index_meta.rs:288`)

**Behaviour.**
- **`prepare_commit` restarts every worker.** It replaces the document channel, **joins every worker thread**, and respawns each one (`index_writer.rs:631-643`). Each worker holding documents therefore flushes its current segment "regardless of the size of their current segment" (doc comment, `index_writer.rs:600-603`). A worker with no documents creates no segment (it peeks first, `index_writer.rs:430-444`). The commit opstamp is stamped after the join (`index_writer.rs:645`).
- **`commit_future` does the rest on the single segment-updater thread** (`src/indexer/segment_updater.rs:442-456`):
  1. `purge_deletes` applies every queued delete to every segment, writing a new `.del` file where something changed (§4).
  2. It moves the segments to committed.
  3. `save_metas` sorts segments by `max_doc` descending (`segment_updater.rs:407`), then calls `directory.sync_directory()` and `atomic_write("meta.json")` (`segment_updater.rs:49-50`).
  4. **It runs garbage collection right away** (`segment_updater.rs:452`).
  5. It asks the merge policy for merges (`segment_updater.rs:453`).

  The source warns: "contrary to what `PrepareCommit` may suggests, this operation is not at all really light" (`prepared_commit.rs:44-46`).
- **The durability cost is fsyncs.** On `MmapDirectory`, every segment file calls `sync_data` when it is finalized (`src/directory/mmap_directory/mod.rs:334-336`). Each new segment has up to 6 component files: postings, positions, fast fields, fieldnorms, terms and store (`src/index/segment_component.rs:9-29`). `atomic_write` writes a temp file, calls `sync_data` and renames it (`mmap_directory/mod.rs:354-367`). `sync_directory` opens the directory and calls `sync_data` (`mmap_directory/mod.rs:513-521`). Separately, each new managed file rewrites `.managed.json` through the same fsync'd `atomic_write` when the file is *created* (`open_write`, `src/directory/managed_directory.rs:213-240,285-287`). That is a cost **per new segment**, paid whenever a worker starts a segment: at commit under `unit` cadence, during indexing under `end` cadence. So a commit costs about (workers holding documents × 6) `sync_data` calls, plus `sync_directory` and the `meta.json` write. Every new segment adds 6 `.managed.json` rewrites. Under per-unit commits the two coincide: about (workers × 12) + 2 fsyncs per commit. GC and merge planning come on top of that. An independent report agrees: "Tantivy ends each segment file and each atomic write (`meta.json` and `.managed.json`) with `File::sync_data`". It measured about 3.7 ms per `sync_data` on macOS APFS, and cut commit durability time from 134 ms to 46 ms with a barrier-fsync `Directory` wrapper (ivygrep PR #432, §9).
- **Measured cost** (`../harness/04-commit-cadence.md`, 4 800 documents in 510 units, WSL2):
  - A per-unit commit costs about **18 ms at 1 thread** (9.2 s in total, against 106 ms committed once).
  - It costs about **66 ms at 16 threads** (33.9 s, against 248 ms).
  - More threads cost more per commit, because each one flushes its own segment.
- **Payload.** `set_payload` stores an arbitrary string in `meta.json` next to the opstamp (`segment_updater.rs:408-414`). It is the natural place to record which generation or snapshot a commit is.
- **`rollback` and `abort`.**
  - `rollback` kills the segment updater, so pending segment updates are ignored (`index_writer.rs:568`).
  - It builds a fresh writer on the same lock (`index_writer.rs:577`).
  - It drains the queued documents without indexing them (`index_writer.rs:590-592`).
  - It returns the last committed opstamp.
  - `PreparedCommit::abort` is the same as `rollback` (`prepared_commit.rs:32-34`).
- **`delete_all_documents`** removes all segments from the updater's registry and reverts the stamper to the committed opstamp. It still "Requires `commit`ing" (`index_writer.rs:503-527`).

**Applies to codectx?** Yes. This is the biggest fixed cost in the build.
- A per-unit seal maps to per-unit `commit()` only at a high price: about 18–66 ms per unit.
- The closer equivalent of "seal each unit, activate once" is: `run(...)` per unit, then one `prepare_commit` + `set_payload(<generation id>)` + `commit` at activation.
- If per-unit durability is truly needed, commit every K units, and measure what that cadence costs.

**Metric.** Index time (dominated by commits × threads), disk (segment count before merging), and crash-recovery granularity.

**Benchmark variants** (all existing flags):
- `tindex docs.jsonl ix-c1-unit 1 default unit` vs `… 1 default end`.
- `tindex docs.jsonl ix-c8-unit 8 default unit` vs `… 8 default every:64` vs `… 8 default end`.
- Report the `-add`, `-final` and total phases separately. The commit share is (unit − end) ÷ number of commits.

---

## 4. Deletes: delete_term and delete_query

**What it is.** A delete is a query, recorded with an opstamp, that is applied lazily.

**Exact API.**
- `delete_term(&self, term: Term) -> Opstamp` (`index_writer.rs:680`)
- `delete_query(&self, query: Box<dyn Query>) -> Result<Opstamp>`. It is marked `#[doc(hidden)]` (`index_writer.rs:697-698`) but is `pub` and callable.
- `UserOperation::Delete(Term)` inside `run`.

**Behaviour.**
- **`delete_term` is a term query.** It wraps `TermQuery::new(term, IndexRecordOption::Basic)` (`index_writer.rs:681`). **A term that is invalid for the schema is silently ignored**: "if Term is invalid for the index, do nothing but return an Opstamp" (`index_writer.rs:682-686`). Check that the field is indexed before relying on it.
- **`delete_query`** builds the weight with scoring disabled, stamps it and pushes it onto the delete queue (`index_writer.rs:699-708`). Any query can be a delete, for example a `TermQuery` on `unit`, or a range on a fast field.
- **Opstamp ordering.** A delete affects only documents added before it: `doc_opstamp < delete_opstamp` (`src/indexer/doc_opstamp_mapping.rs:30-37`). "Delete operation only affects documents that were added in previous commits, and documents that were added previously in the same commit" (`index_writer.rs:672-676`). So delete(unit) followed by add(unit's new docs) in one commit is safe.
- **When a delete applies.**
  - On a freshly flushed segment, deletes are applied before the segment is registered (`apply_deletes`, `index_writer.rs:229-264`).
  - On older segments, deletes apply at commit (`purge_deletes` → `advance_deletes`, `index_writer.rs:131-180`).
  - When there are new deletes, a new alive-bitset file `segment_id.<commit_opstamp>.del` is written (`index_writer.rs:168-174`, ARCHITECTURE.md "Deletes"). Like every segment file, it is immutable.
- **Deleted documents stay physically present until a merge.** They remain in `max_doc`, the postings, fieldnorms and the docstore. The stock BM25 statistics count them (`../harness/06-extension-points.md` cites `src/query/bm25.rs:38-44` and `src/core/searcher.rs:133-141`). Only a merge drops them: the merger maps only `doc_ids_alive()` into the new segment (`src/indexer/merger.rs:244-262`). In tantivy terms, a delete is already "soft" until a merge.
- **No primary key.** "Tantivy does not offer any notion of primary id, so it is up to the user to use a field in their schema as if it was a primary id" (ARCHITECTURE.md, "Deletes").

**Applies to codectx?** Yes.
- Delta rebuild of a unit is one `delete_term(Term::from_field_text(unit, U))`, since `unit` is `STRING | STORED | FAST` in the harness schema, followed by the unit's adds.
- A per-document delete uses the `id` field.
- **Retained snapshots with soft deletes: the two models differ.** Tantivy's model is "only the current commit": a delete is invisible to new searchers after the commit, a merge removes it for good, and GC deletes superseded files at once. That model buys bounded disk and no bookkeeping. codectx's retained snapshots buy reproducible answers for older generations. On tantivy they need an older commit whose files (including its own `.del`) are kept alive by the caller. `04-directories-storage.md` §4 gives three designs and what each costs.
- Product mode already reads the alive bitset, so it computes df, N and avgdl over live documents only (`../harness/06-extension-points.md`).

**Metric.** Index time (delta rebuild), disk (a `.del` file per segment touched, plus dead documents until merge), and quality (stock BM25 drifts while deleted documents remain).

**Benchmark variants.**
- Existing: `tdelta docs.jsonl ix unit`, with phases `delta-put`, `delta-commit`, `delta`, `delta-merge`.
- Add a `delete_query(TermQuery(unit))` path, and a path that deletes per document by `id`, to compare against `delete_term(unit)`. Needs a harness flag (`--delete unit|id|query`).

---

## 5. Merge policies

**What it is.** After every segment change, the segment updater asks the policy for merge candidates. It does this after adding a segment, after a commit and after a merge ends (`segment_updater.rs:352,453,669`). Committed and uncommitted segments are planned separately. A lone segment with no deletes is never a candidate (`segment_updater.rs:576-612`).

**Exact API.**
- `IndexWriter::set_merge_policy(&self, Box<dyn MergePolicy>)` and `get_merge_policy()` (`index_writer.rs:465-472`)
- `trait MergePolicy: Send + Sync + Debug { fn compute_merge_candidates(&self, segments: &[SegmentMeta]) -> Vec<MergeCandidate>; }` with `MergeCandidate(pub Vec<SegmentId>)` (`src/indexer/merge_policy.rs:8,14-20`). The doc comment says this "call happens on the segment updater thread, and will block other segment updates, so all implementations should happen rapidly."
- `NoMergePolicy` (`merge_policy.rs:24-36`)
- `LogMergePolicy` (`src/indexer/log_merge_policy.rs`), which is also `DefaultMergePolicy` (`src/indexer/mod.rs:49`)
- `SegmentMeta::{id(), max_doc(), num_docs(), num_deleted_docs(), delete_opstamp()}` (`src/index/index_meta.rs:81,130,135,86,141`) are what a custom policy sees.

**LogMergePolicy knobs and defaults** (`log_merge_policy.rs:8-15,33-77,130-139`):

| setter | default | meaning |
|---|---|---|
| `set_min_num_segments(usize)` | 8 | a level merges once it holds at least this many segments |
| `set_max_docs_before_merge(usize)` | 10 000 000 | segments with more `num_docs` are left out of planning (merging many smaller ones can still exceed it) |
| `set_min_layer_size(u32)` | 10 000 | sizes below this count as this size, so all small segments share one level |
| `set_level_log_size(f64)` | 0.75 | the log2 width of a level |
| `set_del_docs_ratio_before_merge(f32)` | 1.0 | a level with any segment above this deleted ratio merges anyway. It panics outside (0, 1]. "The default value of 1 means that deletes are not taken in account when identifying merge candidates. This is not a very sensible default" (`log_merge_policy.rs:12-15`) |

- **Algorithm** (`log_merge_policy.rs:94-127`):
  1. Filter out segments with `num_docs` > max.
  2. Sort by `max_doc` descending.
  3. Group consecutive segments while log2(max(num_docs, min_layer)) stays within `level_log_size` of the group's top.
  4. Emit each group with at least `min_num_segments` segments, or with a segment over the delete ratio.

**Custom policy.** Implement the trait. Example for codectx: "merge everything into one segment once there are at least two segments, or once any segment has deletes".

**Applies to codectx?** Yes, on three counts.
1. **Segment count.** Every query walks every segment, and product mode does a per-segment fold plus a cross-segment string fold, so segment count drives query latency.
2. **Deterministic layout.** `NoMergePolicy` plus one explicit `merge(&all_ids)` at activation gives exactly one segment. That removes the scheduling-dependent layout of default background merging (§7).
3. **Retained snapshots.** Merges rewrite live documents into new files and drop the deleted ones. A snapshot that must still show soft-deleted documents has to pin the pre-merge files (see `04`).

Raising `del_docs_ratio` sensitivity, for example to 0.2, keeps delta-rebuilt indexes from carrying dead documents forever. That matters for the stock-BM25 library modes, whose statistics include deleted documents.

**Metric.** Query latency (segments), disk (dead documents and transient double space during merges), index time (merge CPU), and index memory (merge threads).

**Benchmark variants** (all need a harness flag `--merge`):
- `--merge log` (default) vs `--merge none` vs `--merge none+final` (`NoMergePolicy`, then `merge(searchable_segment_ids)` and `wait_merging_threads`), at `tindex docs.jsonl ix 8 default unit` and `… 8 default end`.
- `--merge log:min=2` (`set_min_num_segments(2)`) and `--merge log:del=0.2` (`set_del_docs_ratio_before_merge(0.2)`), with `tdelta` afterwards.
- Record segments at end, the `-final` time, disk bytes, and `tquery` median/p95 per mode.

---

## 6. Merge threads, explicit merges and waiting

**Exact API.**
- `IndexWriterOptions::num_merge_threads` (default 4, `index_writer.rs:60-62`). It became configurable in 0.24: "Make `NUM_MERGE_THREADS` configurable" (CHANGELOG, 0.24).
- `IndexWriter::merge(&mut self, &[SegmentId]) -> FutureResult<Option<SegmentMeta>>` (`index_writer.rs:535`). "If all segments are empty no new segment will be created."
- `IndexWriter::wait_merging_threads(self) -> Result<()>` (`index_writer.rs:354`). It consumes the writer.
- `garbage_collect_files(&self)` (`index_writer.rs:482`)
- Hidden/advanced: `merge_indices(&[Index], dir)` and `merge_filtered_segments(&[Segment], IndexSettings, Vec<Option<AliveBitSet>>, dir)` (`segment_updater.rs:142,188`). Neither checks for a running writer.

**Behaviour.**
- Merges run on a pool named `merge_thread_{i}` with `num_merge_threads` threads (`segment_updater.rs:290-292`). All bookkeeping is serialized on one `segment_updater` thread (`segment_updater.rs:281-283`).
- A merge first advances deletes on its inputs to the target opstamp (`segment_updater.rs:103-107`). It writes the new segment by stacking the inputs' alive documents in input order (`merger.rs:244-262`).
- If deletes were committed during the merge, `end_merge` writes a fresh `.del` for the merged segment (`segment_updater.rs:630-656`). If the result is committed, it rewrites `meta.json` (`segment_updater.rs:664-667`), then runs GC (`segment_updater.rs:672`).
- `wait_merging_thread` blocks until no merge operation is in flight, including merges that cascade from earlier ones (`segment_updater.rs:679-697`). "Terminating your application without letting merge terminate is perfectly safe."
- **Merges continue after `commit()` returns.** A commit-timing measurement that omits `wait_merging_threads` under-counts the build. The harness's `-final` phase already includes it.
- Segment limit: `MAX_DOC_LIMIT = 1 << 31` documents per segment (`merger.rs:27`).

**Applies to codectx?** Yes.
- `num_merge_threads` caps background CPU, in line with the "never freezes the host" constraint.
- An explicit `merge` plus `wait_merging_threads` at activation is the deterministic finish.

**Metric.** Index time (`-final` phase), CPU share and peak RSS during merges, and query latency afterwards.

**Benchmark variant.** `num_merge_threads ∈ {1, 2, 4}` at `tindex docs.jsonl ix 8 default every:64`. Needs a harness flag (`--merge-threads N`). Report the `-final` phase and peak RSS.

---

## 7. Segment counts and determinism of layout

**Behaviour.**
- **How segments are made.** A segment comes from each worker flush: at budget − 1 MB, or at any commit for a worker holding documents.
- **Segment counts drift.** The count after merges depends on the policy and on timing. The smoke ended with 6 segments at 1 thread and 2 at 16 after 510 per-unit commits, and with 1 or 2 when committed once (`../harness/04-commit-cadence.md`).
- **Segment ids are random.** They are random UUIDv4s (`src/index/segment_id.rs:43-48`).
- **Order among equal-sized segments is not fixed.** The register is a `HashMap` (`src/indexer/segment_register.rs:18`). `save_metas` orders segments only by `max_doc` descending (`segment_updater.rs:407`), so segments with equal `max_doc` keep hash order.
- **Consequence.** Neither `DocAddress` nor segment order is a stable identity across rebuilds, or even across runs of the same build with more than one thread.
- **What each side buys.** Tantivy's free assignment of documents to segments buys parallel indexing with no coordination. codectx's deterministic ranking buys identical result lists across rebuilds. On tantivy, determinism therefore comes from the query side. Ties must break on a stored key. Product mode's comparator orders by score, then by exact strings (`../harness/06-extension-points.md`). The library modes' "different tie rules" are listed as a departure in `../harness/00-library-arm.md`.

**Applies to codectx?** Yes, directly. It decides how the deterministic-ranking decision is implemented: a comparator over stored keys, which costs one string comparison per tie; or single-threaded, single-segment builds, which cost index time.

**Metric.** Quality and determinism (identical result lists across rebuilds).

**Benchmark variant.**
1. Build the same corpus twice at `tindex … 8 default end`.
2. Run `tquery … product` and `… library-all` on both.
3. Run `tcompare a.jsonl b.jsonl`. Product mode must give 200/200 and 10/10 overlap with the identical order. Any difference in the library modes is the measured tie nondeterminism.

---

## 8. SingleSegmentIndexWriter (one-shot build)

**What it is.** A hidden writer that builds exactly one segment on the calling thread. It has no queue, no merges and no GC.

**Exact API.** `IndexBuilder::single_segment_index_writer(dir, mem_budget) -> Result<SingleSegmentIndexWriter<D>>` (`src/index/index.rs:184-193`, `#[doc(hidden)]`). Its methods are `add_document(&mut self, D)`, `mem_usage()` and `finalize(self) -> Result<Index>` (`src/indexer/single_segment_index_writer.rs:29-55`).

**Behaviour.**
- **It never flushes.** `add_document` writes straight into one `SegmentWriter`, and the budget only sizes the initial hash table (`single_segment_index_writer.rs:18-38`, `segment_writer.rs:27`). Memory grows with the corpus, up to the about-4 GB arena ceiling.
- **`finalize`** writes `meta.json` with opstamp 0 and syncs the directory (`single_segment_index_writer.rs:40-55`).
- It expects "an originally empty directory, and will not run any GC operation" (`index.rs:183`).

**Applies to codectx?** Partly.
- It gives one segment and deterministic DocIds (arrival order) with no commit or merge cost, which suits a full rebuild of a small or medium repository.
- It is unbounded in memory, single-threaded, and has no delete or delta path. Against the product requirement that indexing never freezes the host, that costs a hard memory bound on large repositories. A bounded `IndexWriter` with one final merge buys the bound back at the price of merge time.

**Metric.** Index time (no merges), index memory (unbounded), and query latency (one segment).

**Benchmark variant.** A `tindex` mode that uses `single_segment_index_writer(dir, 64 MB)` on the full export, against `tindex … 8 large end` followed by an explicit merge to one segment. Needs a harness flag (`--single-segment`). Run under the existing `MemoryMax=4G` scope, and record peak RSS against corpus bytes.

---

## 9. Web sources consulted (with credibility)

**Primary: the library's authors and maintainers.**
- Paul Masurel (tantivy's creator), "Of tantivy's indexing", July 16 2017, tantivy 0.4: https://fulmicoton.com/posts/behold-tantivy-part2/.
  - "Each thread is working on building its own little segment."
  - "Every thread has a user-defined memory budget."
  - "committing too often will hurt your indexing throughput considerably". Merges are continuously considered (LogMergePolicy).
  - Benchmark: 5 M English Wikipedia documents (8 GB), 4 cores with HT, 4 threads, 94 s, merges disabled, index written to SSD. The design still matches 0.26.2 (§1, §3); the numbers are 2017-era.
- Paul Masurel, "Of tantivy indexing datastructure, the stacker", April 16 2019: https://fulmicoton.com/posts/tantivy-stacker/. "Tantivy's API let's the user specify a memory budget and it is tantivy's duty to stick to it". It describes the arena with 32-bit addresses and buckets of a 32-bit hash plus an address, which matches §1.
- tantivy wiki, "Life of a Segment", 2018, tantivy 0.6: https://github.com/quickwit-oss/tantivy/wiki/Life-of-a-Segment. Segment states, immutable segments, tombstones per commit opstamp, eventual GC. The design is accurate; some names are old.
- Quickwit, "Tantivy 0.22", April 16 2024: https://quickwit.io/blog/tantivy-0.22.
  - "Overall indexing throughput increased by ~40% in a benchmark using the GitHub dataset".
  - Memory while indexing 1.1 GB of HDFS logs fell from 760 MB to 590 MB (docid deltas).
  - The minimum budget rose to 15 MB.
  - Maintainer numbers, with no hardware stated.
- Quickwit, "Tantivy 0.19 is out", January 18 2023: https://quickwit.io/blog/tantivy-0.19. About 50% faster indexing from compressing the docstore on a separate thread (the `docstore_compress_dedicated_thread` default, `02-settings-fields.md` §1).
- tantivy issue #706 (opened by the creator in 2019; closed with PR #765): https://github.com/tantivy-search/tantivy/issues/706. LogMergePolicy ignored deletions, so "a large segment can accumulate many deleted documents yet never get merged". That is the origin of `max_docs_before_merge` and `del_docs_ratio_before_merge` (§5). Only the summary was read.
- tantivy issues #1223, #1224 and #1225: fsync counts on commit, a missing fsync on `atomic_write`, and fewer `sync_directory` calls (fixed in 0.17, CHANGELOG). Titles and summaries only.

**Secondary: other projects' measured reports (workload-specific, useful as direction, not as numbers).**
- ivygrep PR #432, October 8 2026, macOS 26.7.1 on an M5 Max, APFS: https://github.com/bvolpato/ivygrep/pull/432. About 3.7 ms per full fsync against 0.4 ms per barrier fsync. A barrier-fsync `Directory` wrapper cut commit durability time from 134 ms to 46 ms. Measured, with the platform stated; macOS only. On Linux `sync_data` is `fdatasync`, so the harness's WSL2 numbers must be measured, not inferred.
- HarperFast/fulltext PR #46, September 30 2026, tantivy 0.26.2: https://github.com/HarperFast/fulltext/pull/46.
  - Limiting merge threads to 1 cut median ingest by about 25%, and to 2 by about 12% (16 indexes).
  - It keeps the default policy with a 50% deleted-document threshold; 30% saved more disk but "added close-time merge drain".
  - Internal harness, no hardware stated. Its direction supports the §5 and §6 variants.
- tantivy issue #2932, May 15 2026, open, no maintainer reply: https://github.com/quickwit-oss/tantivy/issues/2932. A writer per change with `delete_term` + `add_document` + commit, on default settings, reached about 2 000 segments of about 26 KB and "several seconds to load from cold cache". It agrees with §3 and §7.
- nopaldb issue #178 (search-result summary only): one tantivy commit per document, where each commit fsyncs, made a 10 k-chunk fixture take about 18 minutes; the fix was to batch commits. Unverified detail; the direction is consistent with `../harness/04-commit-cadence.md`.

**Searched for, not found.** A current, reproducible tantivy indexing-throughput benchmark with stated hardware; the newest primary numbers are the release posts above. No maintainer-documented RSS model beyond "budget per thread", and RSS above the budget (queue, merges, allocator) is expected (§1).
